package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/prest/prest/v2/adapters"
	"github.com/prest/prest/v2/adapters/mysql"
	"github.com/prest/prest/v2/adapters/postgres"
	"github.com/prest/prest/v2/adapters/timescaledb"
	"github.com/prest/prest/v2/admission"
	"github.com/prest/prest/v2/config"
	pctx "github.com/prest/prest/v2/context"
	"github.com/prest/prest/v2/controllers"
	"github.com/prest/prest/v2/internal/logsafe"
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
// (miniship-cloud#547). Upstream connects to and pings every registry entry
// here (an entry it cannot reach is served lazily), and probes TimescaleDB
// when there is no registry; at 250 Projects that woke 250 computes against a
// ceiling of 20 concurrently active, before a caller had asked for anything.
// Each adapter is created unconnected and its pool fills on the first call
// that names its Database, which is the property ADR 0017 rests on: a Project
// nobody calls costs nothing. registerDatabases, connectAdapterForDatabase and
// detectAndCreateAdapter stay in the tree, unused, for an upstream rebase.
func New(cfg *config.Prest) (*App, error) {
	registry := adapters.NewRegistry()
	// miniship: nothing is reached at start-up, so nothing is unreachable.
	var unreachable map[string]struct{}

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
		// No registry: one adapter for pg.database; pg.allowed_databases share
		// its host and credentials. miniship: created unconnected.
		adapter := postgres.New(cfg)
		setCurrentDatabase(adapter, cfg.PGDatabase)
		cfg.Adapter = adapter
		if err := registerServedAliases(registry, cfg, adapter); err != nil {
			return nil, err
		}
	} else {
		// Adapter already configured (injected): register it
		if err := registerServedAliases(registry, cfg, cfg.Adapter); err != nil {
			return nil, err
		}
	}

	logServedDatabases(cfg)

	if err := ensureSchemaMigrated(cfg, registry, unreachable); err != nil {
		return nil, err
	}

	if err := ensureQueriesImported(cfg); err != nil {
		return nil, err
	}

	// For multi-database mode, set a default adapter for health checks and schema operations
	if cfg.Adapter == nil {
		cfg.Adapter = defaultAdapter(cfg, registry, unreachable)
	}

	deps := controllers.NewDepsFromConfig(cfg)
	deps.AdapterRegistry = registry // Inject registry into deps
	deps.AdapterName = adapterEngineName(cfg.Adapter)
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
	// route (miniship-cloud#547). pREST v2.4.2 wrapped the router in
	// NewAdapterSelectorMiddleware outside it, where mux has not matched yet
	// and mux.Vars is empty, so every read ran on whichever adapter came back
	// first; upstream now installs it after the match. rest does not install
	// it at all: the handler resolves the path's Database itself, and the
	// middleware stays in the tree, unused, as the removed routes' handlers do.
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

// registerDatabases connects and registers an adapter for every registry
// entry. An unreachable entry gets a lazy adapter and is returned in the
// unreachable set; any other error is fatal, as is every entry being down.
func registerDatabases(cfg *config.Prest, registry adapters.Registry) (map[string]struct{}, error) {
	unreachable := make(map[string]struct{})
	for _, dbConf := range cfg.Databases {
		dbCfg := scopedConfig(cfg, &dbConf)
		adapter, err := connectAdapter(&dbCfg, &dbConf)
		if err != nil {
			// Auth failures, unknown engines and bad URLs are config mistakes, not outages.
			if !adapters.IsUnavailable(err) {
				return nil, err
			}
			slog.Warn("database unreachable at startup; serving lazily", "alias", dbConf.Alias, "err", logsafe.Error(err))
			adapter = lazyAdapterForDatabase(&dbCfg, &dbConf)
			unreachable[dbConf.Alias] = struct{}{}
		}
		if err := registry.Register(dbConf.Alias, adapter); err != nil {
			return nil, err
		}
		slog.Info("registered adapter for database", "alias", dbConf.Alias)
	}
	if len(unreachable) == len(cfg.Databases) {
		return nil, fmt.Errorf("no configured database reachable: %w", adapters.ErrUnavailable)
	}
	return unreachable, nil
}

// registerServedAliases registers the single no-registry adapter under
// pg.database ("prest" when unset) and every pg.allowed_databases name, so the
// adapter selector attaches it, and its schema scope, for each served
// {database}.
func registerServedAliases(registry adapters.Registry, cfg *config.Prest, adapter adapters.Adapter) error {
	alias := cfg.PGDatabase
	if alias == "" {
		alias = "prest" // Use default alias when database name is not set
	}
	seen := map[string]struct{}{alias: {}}
	if err := registry.Register(alias, adapter); err != nil {
		return err
	}
	for _, name := range cfg.AllowedDatabases {
		if _, dup := seen[name]; dup || name == "" {
			continue
		}
		seen[name] = struct{}{}
		if err := registry.Register(name, adapter); err != nil {
			return err
		}
	}
	return nil
}

// logServedDatabases tells operators which database names are reachable when
// there is no registry: pg.database plus pg.allowed_databases. Every other
// name is rejected. Registry mode logs nothing here (each alias is logged on
// registration).
func logServedDatabases(cfg *config.Prest) {
	if cfg.HasDatabaseRegistry() {
		return
	}
	slog.Info("serving databases", "databases", servedDatabases(cfg))
}

// servedDatabases returns pg.database plus pg.allowed_databases without empty
// or duplicate names.
func servedDatabases(cfg *config.Prest) []string {
	seen := make(map[string]struct{})
	var served []string
	for _, name := range append([]string{cfg.PGDatabase}, cfg.AllowedDatabases...) {
		if _, dup := seen[name]; name == "" || dup {
			continue
		}
		seen[name] = struct{}{}
		served = append(served, name)
	}
	return served
}

// defaultAdapter picks the adapter behind requests without {database}, health
// checks and script permissions: the first configured alias that was
// reachable at startup, else the first configured alias.
func defaultAdapter(cfg *config.Prest, registry adapters.Registry, unreachable map[string]struct{}) adapters.Adapter {
	if len(cfg.Databases) == 0 {
		return nil
	}
	alias := cfg.Databases[0].Alias
	for _, dbConf := range cfg.Databases {
		if _, down := unreachable[dbConf.Alias]; !down {
			alias = dbConf.Alias
			break
		}
	}
	adapter, err := registry.Get(alias)
	if err != nil {
		return nil
	}
	return adapter
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
		},
		conf, cfg,
	)
}

// ensureSchemaMigrated runs the startup migrations on every registered alias
// except those in skip (unreachable at startup); they migrate on a later start.
func ensureSchemaMigrated(cfg *config.Prest, registry adapters.Registry, skip map[string]struct{}) error {
	needAuth := cfg.AuthEnabled && cfg.AuthMigrateOnStartup
	needQueries := cfg.QueriesConf.Storage == config.QueriesStorageDatabase && cfg.QueriesConf.MigrateOnStartup
	if !needAuth && !needQueries {
		return nil
	}

	if registry == nil {
		return nil
	}
	// Without a registry the allow-listed names share pg.database's adapter;
	// auth/queries tables belong to pg.database only.
	for _, alias := range registry.GetAll() {
		if !cfg.HasDatabaseRegistry() && alias != cfg.PGDatabase && slices.Contains(cfg.AllowedDatabases, alias) {
			continue
		}
		if _, down := skip[alias]; down {
			slog.Warn("auth/queries table migration skipped: database unreachable at startup", "alias", alias)
			continue
		}
		adapter, err := registry.Get(alias)
		if err != nil || adapter == nil {
			continue
		}
		if ensurer, ok := adapter.(adapters.SystemTableEnsurer); ok {
			if err := runSystemTables(ensurer, alias, needAuth, needQueries); err != nil {
				return err
			}
			continue
		}
		if engineForAlias(cfg, alias) != config.EnginePostgres {
			continue
		}
		db, err := postgres.DB(adapter)
		if err != nil {
			if errors.Is(err, postgres.ErrNotPostgresAdapter) {
				return fmt.Errorf("acquire database connection for startup migration: %w", ErrAdapterNotPostgres)
			}
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
	}
	return nil
}

// engineForAlias returns the engine for a registered alias.
// A matching Databases entry uses that entry's engine, otherwise the root engine.
func engineForAlias(cfg *config.Prest, alias string) string {
	if cfg != nil {
		for i := range cfg.Databases {
			if cfg.Databases[i].Alias == alias {
				return config.EffectiveEngine(&cfg.Databases[i], cfg)
			}
		}
	}
	return config.EffectiveEngine(nil, cfg)
}

func runSystemTables(ensurer adapters.SystemTableEnsurer, alias string, needAuth, needQueries bool) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if alias != "" {
		ctx = context.WithValue(ctx, pctx.DBNameKey, alias)
	}
	if needAuth {
		if err := ensurer.EnsureAuthTable(ctx); err != nil {
			return fmt.Errorf("migrate auth table: %w", err)
		}
		slog.Info("auth table migration complete", "alias", alias)
	}
	if needQueries {
		if err := ensurer.EnsureQueriesTable(ctx); err != nil {
			return fmt.Errorf("migrate queries table: %w", err)
		}
		slog.Info("queries table migration complete", "alias", alias)
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

// EnsureAdapter connects the configured adapter when cfg.Adapter is nil.
func EnsureAdapter(cfg *config.Prest) error {
	if cfg.Adapter != nil {
		return nil
	}
	if config.EffectiveEngine(nil, cfg) == config.EngineMySQL {
		my := mysql.New(cfg)
		if err := mysql.Connect(my); err != nil {
			return err
		}
		cfg.Adapter = my
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

// adapterEngineName reports the default adapter for /_health.
// A nil adapter and test doubles are reported as postgres.
func adapterEngineName(a adapters.Adapter) string {
	switch a.(type) {
	case *mysql.Adapter:
		return "mysql"
	case *timescaledb.Adapter:
		return "timescaledb"
	default:
		return "postgres"
	}
}

// detectAndCreateAdapter tries to connect to TimescaleDB first; if not available, falls back to PostgreSQL.
// This allows pREST to auto-detect and use the appropriate adapter without configuration.
func detectAndCreateAdapter(cfg *config.Prest) (adapters.Adapter, error) {
	if err := cfg.ValidateEngines(); err != nil {
		return nil, err
	}
	if config.EffectiveEngine(nil, cfg) == config.EngineMySQL {
		my := mysql.New(cfg)
		if err := mysql.Connect(my); err != nil {
			return nil, err
		}
		slog.Info("using mysql adapter")
		return my, nil
	}
	// Try TimescaleDB first
	tsAdapter := timescaledb.New(cfg)
	err := timescaledb.Connect(tsAdapter)
	if err == nil {
		slog.Info("detected TimescaleDB; using timescaledb adapter")
		return tsAdapter, nil
	}
	if adapters.IsUnavailable(err) {
		// Unreachable host: a Postgres retry would only wait for a second timeout.
		return nil, err
	}
	// Reachable but not TimescaleDB: fall back to PostgreSQL
	pgAdapter := postgres.New(cfg)
	if err := postgres.Connect(pgAdapter); err != nil {
		return nil, err
	}
	slog.Info("using postgres adapter")
	return pgAdapter, nil
}

// scopedConfig returns a copy of cfg scoped to one registry entry. Pool limits
// are copied only when set, so an entry never passes 0 (unlimited) to
// database/sql; cfg itself is not modified.
func scopedConfig(cfg *config.Prest, dbConf *config.DatabaseConf) config.Prest {
	eng := config.EffectiveEngine(dbConf, cfg)
	dbCfg := *cfg
	dbCfg.Engine = eng
	dbCfg.PGHost = dbConf.Host
	dbCfg.PGPort = dbConf.Port
	dbCfg.PGUser = dbConf.User
	dbCfg.PGPass = dbConf.Pass
	dbCfg.PGDatabase = dbConf.Database
	if dbConf.MaxOpenConn > 0 {
		dbCfg.PGMaxOpenConn = dbConf.MaxOpenConn
	}
	if dbConf.MaxIdleConn > 0 {
		dbCfg.PGMaxIdleConn = dbConf.MaxIdleConn
	}
	dbCfg.PGSSLMode = dbConf.SSL.Mode
	dbCfg.PGSSLCert = dbConf.SSL.Cert
	dbCfg.PGSSLKey = dbConf.SSL.Key
	dbCfg.PGSSLRootCert = dbConf.SSL.RootCert
	if dbConf.URL != "" && (eng != config.EngineMySQL || strings.HasPrefix(strings.ToLower(dbConf.URL), "mysql://")) {
		dbCfg.PGURL = dbConf.URL
	}
	if eng == config.EngineMySQL {
		config.ApplyMySQLRegistrySchemas(&dbCfg, dbConf.Database)
	}
	return dbCfg
}

// connectAdapter is the registry-mode connector; tests replace it to inject adapters.
var connectAdapter = connectAdapterForDatabase

// connectAdapterForDatabase creates and connects an adapter for one registry
// entry, using dbCfg from scopedConfig. Postgres entries are probed as
// TimescaleDB first; an unreachable host is reported without a Postgres retry.
func connectAdapterForDatabase(dbCfg *config.Prest, dbConf *config.DatabaseConf) (adapters.Adapter, error) {
	if dbCfg.Engine == config.EngineMySQL {
		my := mysql.New(dbCfg)
		if err := mysql.Connect(my); err != nil {
			return nil, fmt.Errorf("failed to connect to database %s: %w", dbConf.Alias, err)
		}
		my.SetDatabase(dbConf.Alias)
		slog.Info("using mysql adapter for database", "alias", dbConf.Alias)
		return my, nil
	}
	if dbCfg.Engine != config.EnginePostgres {
		return nil, fmt.Errorf("database %s: unknown database engine %q", dbConf.Alias, dbCfg.Engine)
	}

	// Try TimescaleDB first, fall back to PostgreSQL
	tsAdapter := timescaledb.New(dbCfg)
	err := timescaledb.Connect(tsAdapter)
	if err == nil {
		slog.Info("detected TimescaleDB for database", "alias", dbConf.Alias)
		return tsAdapter, nil
	}
	if adapters.IsUnavailable(err) {
		return nil, fmt.Errorf("failed to connect to database %s: %w", dbConf.Alias, err)
	}

	// Reachable but not TimescaleDB: fall back to PostgreSQL
	pgAdapter := postgres.New(dbCfg)
	if err := postgres.Connect(pgAdapter); err != nil {
		return nil, fmt.Errorf("failed to connect to database %s: %w", dbConf.Alias, err)
	}
	slog.Info("using postgres adapter for database", "alias", dbConf.Alias)
	return pgAdapter, nil
}

// lazyAdapterForDatabase builds an unconnected adapter of the entry's engine;
// its pool dials on first use, so requests recover once the database is up.
// TimescaleDB detection is skipped: hypertable endpoints for the alias stay
// unavailable until restart.
func lazyAdapterForDatabase(dbCfg *config.Prest, dbConf *config.DatabaseConf) adapters.Adapter {
	if dbCfg.Engine == config.EngineMySQL {
		my := mysql.New(dbCfg)
		my.SetDatabase(dbConf.Alias)
		return my
	}
	slog.Warn("timescaledb detection skipped: database unreachable at startup", "alias", dbConf.Alias)
	pg := postgres.New(dbCfg)
	// Same database selection postgres.Connect would have made.
	pg.SetDatabase(dbCfg.PGDatabase)
	return pg
}

// createAdapterForDatabase creates an unconnected adapter for one registry
// entry. Every database uses the postgres adapter.
//
// miniship: two things went from upstream's connectAdapterForDatabase
// (miniship-cloud#547). The adapter is not connected, so composing rest
// touches no Database; and TimescaleDB is not detected, because detecting it
// *is* a connection, one to every entry, at start-up, before the fallback
// opens a second. rest serves a Project's public schema over one grammar, and
// the timescaledb adapter remains in the tree for an upstream rebase.
//
// The entry's own registry is what the adapter is given: this adapter holds
// this Database and can resolve no other, so a read that reached the wrong
// adapter is refused rather than quietly answered from the right connection.
func createAdapterForDatabase(cfg *config.Prest, dbConf *config.DatabaseConf) adapters.Adapter {
	// A config scoped to this database, upstream's scopedConfig.
	dbCfg := scopedConfig(cfg, dbConf)
	dbCfg.Databases = []config.DatabaseConf{*dbConf}

	adapter := postgres.New(&dbCfg)
	setCurrentDatabase(adapter, dbConf.Database)
	slog.Info("using postgres adapter for database", "alias", dbConf.Alias)
	return adapter
}

// setCurrentDatabase names the database an adapter answers for when a caller
// names none.
//
// miniship: upstream sets it as a side effect of connecting at start-up. rest
// does not connect at start-up, so it is set here, and the pool still fills on
// the first call that needs it.
func setCurrentDatabase(adapter adapters.Adapter, name string) {
	if registry, ok := adapter.(adapters.DatabaseRegistry); ok {
		registry.SetDatabase(name)
	}
}
