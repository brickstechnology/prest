package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/prest/prest/v2/adapters"
	"github.com/prest/prest/v2/adapters/postgres"
	"github.com/prest/prest/v2/admission"
	"github.com/prest/prest/v2/config"
	pctx "github.com/prest/prest/v2/context"
	"github.com/prest/prest/v2/controllers"
	"github.com/prest/prest/v2/middlewares"
	"github.com/prest/prest/v2/plugins"
	"github.com/prest/prest/v2/router"

	"github.com/gorilla/mux"
	"github.com/jmoiron/sqlx"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
)

// App is the composition root for the HTTP server.
type App struct {
	Config   *config.Prest
	Handler  http.Handler
	Adapters adapters.Registry
	pg       adapters.Adapter // deprecated: kept for backward compatibility
}

// New builds a ready-to-serve App from cfg.
//
// Creates and registers an adapter for each configured database. If cfg.Adapter
// is nil and no database registry is configured, a default adapter is created.
// Handlers, CRUD middleware, routes, global middleware, and plugins are wired into
// a single http.Handler.
//
// miniship: composing rest opens no connection to any Database
// (miniship-cloud#547). Upstream connected to and pinged every registry entry
// here, and probed TimescaleDB when there was no registry; at 250 Projects that
// woke 250 computes against a ceiling of 20 concurrently active, before a
// caller had asked for anything. Each adapter is created unconnected and its
// pool fills on the first call that names its Database, which is the property
// ADR 0017 rests on: a Project nobody calls costs nothing.
func New(cfg *config.Prest) (*App, error) {
	registry := adapters.NewRegistry()

	// Multi-database mode: create an adapter for each configured database
	if cfg.HasDatabaseRegistry() {
		// The default adapter below is this one, taken in the order the
		// registry is configured in rather than from a map, so which Database
		// answers a question asked of no Database in particular does not
		// change between two runs of the same binary.
		var first adapters.Adapter
		for _, dbConf := range cfg.Databases {
			adapter := createAdapterForDatabase(cfg, &dbConf)
			if err := registry.Register(dbConf.Alias, adapter); err != nil {
				return nil, err
			}
			if first == nil {
				first = adapter
			}
			slog.Info("registered adapter for database", "alias", dbConf.Alias)
		}
		if cfg.Adapter == nil {
			cfg.Adapter = first
		}
	} else if cfg.Adapter == nil {
		// Single database mode (backward compatibility): create the default adapter
		adapter := postgres.New(cfg)
		setCurrentDatabase(adapter, cfg.PGDatabase)
		cfg.Adapter = adapter
		alias := cfg.PGDatabase
		if alias == "" {
			alias = "prest" // Use default alias when database name is not set
		}
		if err := registry.Register(alias, adapter); err != nil {
			return nil, err
		}
	} else {
		// Adapter already configured (injected): register it
		alias := cfg.PGDatabase
		if alias == "" {
			alias = "prest" // Use default alias when database name is not set
		}
		if err := registry.Register(alias, cfg.Adapter); err != nil {
			return nil, err
		}
	}

	if err := ensureSchemaMigrated(cfg); err != nil {
		return nil, err
	}

	if err := ensureQueriesImported(cfg); err != nil {
		return nil, err
	}

	deps := controllers.NewDepsFromConfig(cfg)
	deps.AdapterRegistry = registry // Inject registry into deps
	// miniship: a Project rest was not given is learned about on a miss
	// (miniship-cloud#548). No lookup address is rest as #547 shipped it: the
	// Databases above, and a name that is not one of them refused.
	if gate := admissionGate(cfg, registry); gate != nil {
		deps.Admitter = gate
	}
	h := controllers.NewHandlers(deps, cfg)

	plg := plugins.New(cfg)
	crud := middlewares.NewCRUDStack(cfg, plg)
	queryStack := middlewares.NewQueryStack(cfg, middlewares.ScriptPermsFromAdapter(cfg.Adapter))
	var adminStack *middlewares.AdminQueryStack
	if cfg.QueriesConf.RegisterEnabled && cfg.QueriesConf.Storage == config.QueriesStorageDatabase {
		adminStack = middlewares.NewAdminQueryStack(cfg)
	}

	mux := mux.NewRouter().StrictSlash(true)
	router.RegisterRoutes(mux, cfg, h, crud, queryStack, adminStack, plg)

	// miniship: the adapter is chosen inside the handler, from the matched
	// route (miniship-cloud#547). Upstream wrapped the router in
	// NewAdapterSelectorMiddleware here, outside it, where mux has not matched
	// yet and mux.Vars is empty — so the middleware never saw {database} and
	// every read ran on whichever adapter came back first. The middleware is
	// left in the tree, unused, as the removed routes' handlers are, so an
	// upstream rebase does not conflict on it.
	n := middlewares.New(cfg)
	n.UseHandler(mux)

	var handler http.Handler = n
	if cfg.Otel.Enabled {
		// Outermost span covers the whole middleware chain and extracts inbound
		// W3C trace context. Per-route http.route tags are added in the router.
		handler = otelhttp.NewHandler(handler, "prest")
	}
	return &App{Config: cfg, Handler: handler, Adapters: registry, pg: cfg.Adapter}, nil
}

// admissionGate is how rest learns about a Project it was not given, or nil
// when it was given no address to ask at (miniship-cloud#548).
//
// The gate is given the same registry the configured Projects are in, so an
// admitted Project is found by the same lookup as one rest started with and
// its second call costs nothing; and the same adapter constructor, so it is
// unconnected until the read that admitted it needs it, holds that one
// Database alone, and can resolve no other.
func admissionGate(cfg *config.Prest, registry adapters.Registry) *admission.Gate {
	conf := cfg.Admission.WithDefaults()
	if conf.URL == "" {
		return nil
	}
	return admission.NewGate(
		admission.NewHTTPAnswerer(conf.URL, conf.Key, conf.Timeout),
		registry,
		admission.Pool{
			Open: func(dbConf config.DatabaseConf) (adapters.Adapter, error) {
				return createAdapterForDatabase(cfg, &dbConf), nil
			},
			Close: postgres.Close,
			// A pool replaced by a fresh credential is left alone for the
			// time limit a read is given (#549), so a read that was already
			// running on it has been cancelled by its own bound before it is
			// closed underneath.
			Grace: time.Duration(cfg.PGStatementTimeoutMS) * time.Millisecond,
		},
		conf, cfg,
	)
}

func ensureSchemaMigrated(cfg *config.Prest) error {
	needAuth := cfg.AuthEnabled && cfg.AuthMigrateOnStartup
	needQueries := cfg.QueriesConf.Storage == config.QueriesStorageDatabase && cfg.QueriesConf.MigrateOnStartup
	if !needAuth && !needQueries {
		return nil
	}

	db, err := PostgresDB(cfg)
	if err != nil {
		return fmt.Errorf("acquire database connection for startup migration: %w", err)
	}

	if needAuth {
		if err := EnsureAuthTable(cfg, db); err != nil {
			return fmt.Errorf("migrate auth table %s.%s: %w", cfg.AuthSchema, cfg.AuthTable, err)
		}
		slog.Info("auth table migration complete", "schema", cfg.AuthSchema, "table", cfg.AuthTable)
	}

	if needQueries {
		qc := cfg.QueriesConf
		if err := EnsureQueriesTable(cfg, db); err != nil {
			return fmt.Errorf("migrate queries table %s.%s: %w", qc.Schema, qc.Table, err)
		}
		slog.Info("queries table migration complete", "schema", qc.Schema, "table", qc.Table)
	}

	return nil
}

func ensureQueriesImported(cfg *config.Prest) error {
	qc := cfg.QueriesConf
	if qc.Storage != config.QueriesStorageDatabase || !qc.ImportOnStartup {
		return nil
	}
	queriesPath := cfg.QueriesPath
	if env := os.Getenv("PREST_QUERIES_LOCATION"); env != "" {
		queriesPath = env
	}
	if queriesPath == "" {
		return nil
	}

	registry, ok := cfg.Adapter.(adapters.QueryRegistry)
	if !ok {
		return ErrAdapterNotQueryRegistry
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	ctx = context.WithValue(ctx, pctx.DBNameKey, cfg.PGDatabase)

	report, err := registry.ImportFromFilesystem(ctx, queriesPath, qc.ImportPolicy)
	if err != nil {
		return fmt.Errorf("import query scripts from %s: %w", queriesPath, err)
	}
	slog.Info("queries filesystem import complete",
		"inserted", report.Inserted,
		"updated", report.Updated,
		"skipped", report.Skipped,
		"location", queriesPath)
	return nil
}

// EnsureAdapter connects the postgres adapter when cfg.Adapter is nil.
func EnsureAdapter(cfg *config.Prest) error {
	if cfg.Adapter != nil {
		return nil
	}
	pg := postgres.New(cfg)
	if err := postgres.Connect(pg); err != nil {
		return err
	}
	cfg.Adapter = pg
	return nil
}

// PostgresDB returns a sqlx connection from the configured postgres adapter.
func PostgresDB(cfg *config.Prest) (*sqlx.DB, error) {
	if err := EnsureAdapter(cfg); err != nil {
		return nil, err
	}
	db, err := postgres.DB(cfg.Adapter)
	if err != nil {
		if errors.Is(err, postgres.ErrNotPostgresAdapter) {
			return nil, ErrAdapterNotPostgres
		}
		return nil, err
	}
	return db, nil
}

// createAdapterForDatabase creates an unconnected adapter for one registry
// entry. Every database uses the postgres adapter.
//
// miniship: two things went from here (miniship-cloud#547). The adapter is no
// longer connected, so composing rest touches no Database; and TimescaleDB is
// no longer detected, because detecting it *is* a connection — one to every
// entry, at start-up, before the fallback opens a second. rest serves a
// Project's public schema over one grammar, and the timescaledb adapter
// remains in the tree for an upstream rebase.
//
// The entry's own registry is what the adapter is given: this adapter holds
// this Database and can resolve no other, so a read that reached the wrong
// adapter is refused rather than quietly answered from the right connection.
func createAdapterForDatabase(cfg *config.Prest, dbConf *config.DatabaseConf) adapters.Adapter {
	// Create a temporary config scoped to this database for adapter creation
	dbCfg := *cfg
	dbCfg.Databases = []config.DatabaseConf{*dbConf}
	dbCfg.PGHost = dbConf.Host
	dbCfg.PGPort = dbConf.Port
	dbCfg.PGUser = dbConf.User
	dbCfg.PGPass = dbConf.Pass
	dbCfg.PGDatabase = dbConf.Database
	dbCfg.PGMaxOpenConn = dbConf.MaxOpenConn
	dbCfg.PGMaxIdleConn = dbConf.MaxIdleConn
	dbCfg.PGSSLMode = dbConf.SSL.Mode
	dbCfg.PGSSLCert = dbConf.SSL.Cert
	dbCfg.PGSSLKey = dbConf.SSL.Key
	dbCfg.PGSSLRootCert = dbConf.SSL.RootCert
	if dbConf.URL != "" {
		dbCfg.PGURL = dbConf.URL
	}

	adapter := postgres.New(&dbCfg)
	setCurrentDatabase(adapter, dbConf.Database)
	slog.Info("using postgres adapter for database", "alias", dbConf.Alias)
	return adapter
}

// setCurrentDatabase names the database an adapter answers for when a caller
// names none.
//
// miniship: upstream set it as a side effect of connecting at start-up. rest
// does not connect at start-up, so it is set here, and the pool still fills on
// the first call that needs it.
func setCurrentDatabase(adapter adapters.Adapter, name string) {
	if registry, ok := adapter.(adapters.DatabaseRegistry); ok {
		registry.SetDatabase(name)
	}
}
