package command

import (
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/src-d/gitbase"
	"github.com/src-d/gitbase/internal/function"
	"github.com/src-d/gitbase/internal/rule"

	"github.com/opentracing/opentracing-go"
	gopilosa "github.com/pilosa/go-pilosa"
	"github.com/sirupsen/logrus"
	"github.com/uber/jaeger-client-go/config"
	sqle "gopkg.in/src-d/go-mysql-server.v0"
	"gopkg.in/src-d/go-mysql-server.v0/server"
	"gopkg.in/src-d/go-mysql-server.v0/sql"
	"gopkg.in/src-d/go-mysql-server.v0/sql/analyzer"
	"gopkg.in/src-d/go-mysql-server.v0/sql/index/pilosa"
	"gopkg.in/src-d/go-mysql-server.v0/sql/index/pilosalib"
	"gopkg.in/src-d/go-vitess.v0/mysql"
)

const (
	ServerDescription = "Starts a gitbase server instance"
	ServerHelp        = ServerDescription + "\n\n" +
		"By default when gitbase encounters an error in a repository it\n" +
		"stops the query. With GITBASE_SKIP_GIT_ERRORS variable it won't\n" +
		"complain and just skip those rows or repositories."
	TracerServiceName = "gitbase"
)

// Server represents the `server` command of gitbase cli tool.
type Server struct {
	Verbose       bool     `short:"v" description:"Activates the verbose mode"`
	Directories   []string `short:"d" long:"directories" description:"Path where the git repositories are located (standard and siva), multiple directories can be defined. Accepts globs."`
	Depth         int      `long:"depth" default:"1000" description:"load repositories looking at less than <depth> nested subdirectories."`
	DisableGit    bool     `long:"no-git" description:"disable the load of git standard repositories."`
	DisableSiva   bool     `long:"no-siva" description:"disable the load of siva files."`
	Host          string   `long:"host" default:"localhost" description:"Host where the server is going to listen"`
	Port          int      `short:"p" long:"port" default:"3306" description:"Port where the server is going to listen"`
	User          string   `short:"u" long:"user" default:"root" description:"User name used for connection"`
	Password      string   `short:"P" long:"password" default:"" description:"Password used for connection"`
	PilosaURL     string   `long:"pilosa" default:"http://localhost:10101" description:"URL to your pilosa server" env:"PILOSA_ENDPOINT"`
	IndexDir      string   `short:"i" long:"index" default:"/var/lib/gitbase/index" description:"Directory where the gitbase indexes information will be persisted." env:"GITBASE_INDEX_DIR"`
	DisableSquash bool     `long:"no-squash" description:"Disables the table squashing."`
	TraceEnabled  bool     `long:"trace" env:"GITBASE_TRACE" description:"Enables jaeger tracing"`
	ReadOnly      bool     `short:"r" long:"readonly" description:"Only allow read queries. This disables creating and deleting indexes as well." env:"GITBASE_READONLY"`
	// SkipGitErrors disables failing when Git errors are found.
	SkipGitErrors bool
	// Version of the application.
	Version string

	engine *sqle.Engine
	pool   *gitbase.RepositoryPool
	name   string
}

type jaegerLogrus struct {
	*logrus.Entry
}

func (l *jaegerLogrus) Error(s string) {
	l.Entry.Error(s)
}

// Execute starts a new gitbase server based on provided configuration, it
// honors the go-flags.Commander interface.
func (c *Server) Execute(args []string) error {
	if c.Verbose {
		logrus.SetLevel(logrus.DebugLevel)
	}

	if err := c.buildDatabase(); err != nil {
		logrus.WithField("error", err).Fatal("unable to start database server")
		return err
	}

	auth := mysql.NewAuthServerStatic()
	auth.Entries[c.User] = []*mysql.AuthServerStaticEntry{
		{Password: c.Password},
	}

	var tracer opentracing.Tracer
	if c.TraceEnabled {
		cfg, err := config.FromEnv()
		if err != nil {
			logrus.WithField("error", err).
				Fatal("unable to read jaeger environment")
			return err
		}

		if cfg.ServiceName == "" {
			cfg.ServiceName = TracerServiceName
		}

		logger := &jaegerLogrus{logrus.WithField("subsystem", "jaeger")}

		t, closer, err := cfg.NewTracer(
			config.Logger(logger),
		)

		if err != nil {
			logrus.WithField("error", err).Fatal("unable to initialize tracer")
			return err
		}

		tracer = t
		defer closer.Close()

		logrus.Info("tracing enabled")
	}

	hostString := net.JoinHostPort(c.Host, strconv.Itoa(c.Port))
	s, err := server.NewServer(
		server.Config{
			Protocol: "tcp",
			Address:  hostString,
			Auth:     auth,
			Tracer:   tracer,
		},
		c.engine,
		gitbase.NewSessionBuilder(c.pool,
			gitbase.WithSkipGitErrors(c.SkipGitErrors),
		),
	)
	if err != nil {
		return err
	}

	logrus.Infof("server started and listening on %s:%d", c.Host, c.Port)
	return s.Start()
}

func (c *Server) buildDatabase() error {
	if c.engine == nil {
		catalog := sql.NewCatalog()
		ab := analyzer.NewBuilder(catalog)
		if c.ReadOnly {
			ab = ab.ReadOnly()
		}
		a := ab.Build()
		c.engine = sqle.New(catalog, a, &sqle.Config{
			VersionPostfix: c.Version,
		})
	}

	c.pool = gitbase.NewRepositoryPool()

	if err := c.addDirectories(); err != nil {
		return err
	}

	c.engine.AddDatabase(gitbase.NewDatabase(c.name))
	logrus.WithField("db", c.name).Debug("registered database to catalog")

	c.engine.Catalog.RegisterFunctions(function.Functions)
	logrus.Debug("registered all available functions in catalog")

	if err := c.registerDrivers(); err != nil {
		return err
	}

	if !c.DisableSquash {
		logrus.Info("squash tables rule is enabled")
		a := analyzer.NewBuilder(c.engine.Catalog).
			AddPostAnalyzeRule(rule.SquashJoinsRule, rule.SquashJoins).
			Build()

		a.CurrentDatabase = c.engine.Analyzer.CurrentDatabase
		c.engine.Analyzer = a
	} else {
		logrus.Warn("squash tables rule is disabled")
	}

	return c.engine.Init()
}

func (c *Server) registerDrivers() error {
	if err := os.MkdirAll(c.IndexDir, 0755); err != nil {
		return err
	}

	logrus.Debug("created index storage")

	client, err := gopilosa.NewClient(c.PilosaURL)
	if err != nil {
		return err
	}

	logrus.Debug("established connection with pilosa")

	c.engine.Catalog.RegisterIndexDriver(pilosa.NewDriver(c.IndexDir, client))
	c.engine.Catalog.RegisterIndexDriver(pilosalib.NewDriver(c.IndexDir))
	logrus.Debug("registered pilosa index driver")

	return nil
}

func (c *Server) addDirectories() error {
	if len(c.Directories) == 0 {
		logrus.Error("At least one folder should be provided.")
	}

	if c.DisableGit && c.DisableSiva {
		logrus.Warn("The load of git repositories and siva files are disabled," +
			" no repository will be added.")

		return nil
	}

	if c.Depth < 1 {
		logrus.Warn("--depth flag set to a number less than 1," +
			" no repository will be added.")

		return nil
	}

	for _, directory := range c.Directories {
		if err := c.addDirectory(directory); err != nil {
			return err
		}
	}

	return nil
}

func (c *Server) addDirectory(directory string) error {
	matches, err := gitbase.PatternMatches(directory)
	if err != nil {
		return err
	}

	for _, match := range matches {
		if err := c.addMatch(match); err != nil {
			logrus.WithFields(logrus.Fields{
				"path":  match,
				"error": err,
			}).Error("path couldn't be inspected")
		}
	}

	return nil
}

func (c *Server) addMatch(match string) error {
	root, err := filepath.Abs(match)
	if err != nil {
		return err
	}

	initDepth := strings.Count(root, string(os.PathSeparator))
	return filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		if info.IsDir() {
			if err := c.addIfGitRepo(path); err != nil {
				return err
			}

			depth := strings.Count(path, string(os.PathSeparator)) - initDepth
			if depth >= c.Depth {
				return filepath.SkipDir
			}

			return nil
		}

		if !c.DisableSiva &&
			info.Mode().IsRegular() && gitbase.IsSivaFile(info.Name()) {
			if err := c.pool.AddSivaFileWithID(info.Name(), path); err != nil {
				logrus.WithFields(logrus.Fields{
					"path":  path,
					"error": err,
				}).Error("repository could not be addded")

				return nil
			}

			logrus.WithField("path", path).Debug("repository added")
		}

		return nil
	})
}

func (c *Server) addIfGitRepo(path string) error {
	ok, err := gitbase.IsGitRepo(path)
	if err != nil {
		logrus.WithFields(logrus.Fields{
			"path":  path,
			"error": err,
		}).Error("path couldn't be inspected")

		return filepath.SkipDir
	}

	if ok {
		if !c.DisableGit {
			base := filepath.Base(path)
			if err := c.pool.AddGitWithID(base, path); err != nil {
				logrus.WithFields(logrus.Fields{
					"id":    base,
					"path":  path,
					"error": err,
				}).Error("repository could not be added")
			}

			logrus.WithField("path", path).Debug("repository added")
		}

		// either the repository is added or not, the path must be skipped
		return filepath.SkipDir
	}

	return nil
}
