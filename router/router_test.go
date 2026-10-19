package router

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/golang/mock/gomock"
	"github.com/gorilla/mux"
	"github.com/stretchr/testify/require"

	"github.com/prest/prest/v2/adapters/mockgen"
	"github.com/prest/prest/v2/config"
	"github.com/prest/prest/v2/controllers"
	"github.com/prest/prest/v2/middlewares"
	"github.com/prest/prest/v2/plugins"
)

const testRouterJWTKey = "router-test-secret-key-at-least-32-bytes!"

// routerFixture holds the mux plus the mocks a test may set expectations on.
type routerFixture struct {
	mux     *mux.Router
	adapter *mockgen.MockAdapter
	builder *mockgen.MockRequestQueryBuilder
	db      *mockgen.MockDatabaseRegistry
	pinger  *mockgen.MockDatabasePinger
}

// newTestRouter wires RegisterRoutes exactly as app.go does, but with mock
// ports so no database is touched. Unauthenticated requests must be rejected
// before any handler runs, so the mocks carry no expectations by default.
func newTestRouter(t *testing.T, authEnabled bool) *routerFixture {
	t.Helper()
	return newTestRouterWithRegistry(t, authEnabled, false)
}

// newTestRouterWithRegistry also enables telemetry route tagging and the
// database-backed query registry (adminStack) when withRegistry is true.
func newTestRouterWithRegistry(t *testing.T, authEnabled, withRegistry bool) *routerFixture {
	t.Helper()
	ctrl := gomock.NewController(t)
	f := &routerFixture{
		adapter: mockgen.NewMockAdapter(ctrl),
		builder: mockgen.NewMockRequestQueryBuilder(ctrl),
		db:      mockgen.NewMockDatabaseRegistry(ctrl),
		pinger:  mockgen.NewMockDatabasePinger(ctrl),
	}
	cfg := &config.Prest{
		AuthEnabled: authEnabled,
		JWTKey:      testRouterJWTKey,
		JWTAlgo:     "HS256",
		PluginPath:  t.TempDir(),
		Adapter:     f.adapter,
	}
	if withRegistry {
		cfg.Otel.Enabled = true
		cfg.QueriesConf.RegisterEnabled = true
		cfg.QueriesConf.Storage = config.QueriesStorageDatabase
	}
	deps := controllers.Deps{
		Catalog:     mockgen.NewMockCatalogQuerier(ctrl),
		Builder:     f.builder,
		Executor:    mockgen.NewMockQueryExecutor(ctrl),
		SQL:         mockgen.NewMockSQLBuilder(ctrl),
		Perms:       f.adapter,
		Scripts:     mockgen.NewMockScriptRunner(ctrl),
		DB:          f.db,
		Pinger:      f.pinger,
		Readiness:   mockgen.NewMockReadinessChecker(ctrl),
		AdapterName: "postgres",
		PGDatabase:  "prest-test",
	}
	var adminStack *middlewares.AdminQueryStack
	if withRegistry {
		deps.QueryRegistry = mockgen.NewMockQueryRegistry(ctrl)
		adminStack = middlewares.NewAdminQueryStack(cfg)
	}
	h := controllers.NewHandlers(deps, cfg)
	plg := plugins.New(cfg)
	crudStack := middlewares.NewCRUDStack(cfg, plg)
	queryStack := middlewares.NewQueryStack(cfg, middlewares.ScriptPermsFromAdapter(cfg.Adapter))

	f.mux = mux.NewRouter().StrictSlash(true)
	RegisterRoutes(f.mux, cfg, h, crudStack, queryStack, adminStack, plg)
	return f
}

func serve(f *routerFixture, method, target, token string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	f.mux.ServeHTTP(rec, req)
	return rec
}

// miniship: upstream v2.5.2 puts its catalog routes, /_PLUGIN and /_mcp behind
// an auth stack (GHSA-fhf5-m388-4w4p, GHSA-fmr6-9j48-xrqp) and keeps the
// stored-query registry behind adminRoute (GHSA-2x9q-r7j9-g85f). rest does not
// register any of them (patch 4), so these subjects hold the stronger answer:
// with auth on or off, with a token or without one, every one of those
// addresses is not found. router/surface_test.go walks the route table; these
// send the advisories' own requests through it.

// advisoryTargets is every request the three advisories make, as upstream's
// own router tests list them. A GET of three segments is rest's table read,
// {database}/{schema}/{table}, which takes /_PLUGIN/hello/Hello as a Database
// called _PLUGIN; tableRead marks those, and no plugin, catalog or query code
// is behind them.
var advisoryTargets = []struct {
	description string
	method      string
	target      string
	tableRead   bool
}{
	{description: "list databases (GHSA-fhf5)", method: http.MethodGet, target: "/databases"},
	{description: "list schemas (GHSA-fhf5)", method: http.MethodGet, target: "/schemas"},
	{description: "list tables (GHSA-fhf5)", method: http.MethodGet, target: "/tables"},
	{description: "list tables of database/schema (GHSA-fhf5)", method: http.MethodGet, target: "/prest-test/public"},
	{description: "show table structure (GHSA-fhf5)", method: http.MethodGet, target: "/show/prest-test/public/test"},
	{description: "plugin handler (GHSA-fmr6)", method: http.MethodGet, target: "/_PLUGIN/hello/Hello", tableRead: true},
	{description: "MCP POST", method: http.MethodPost, target: "/_mcp"},
	{description: "MCP GET", method: http.MethodGet, target: "/_mcp"},
	{description: "CRUD insert", method: http.MethodPost, target: "/prest-test/public/test"},
	{description: "batch insert", method: http.MethodPost, target: "/batch/prest-test/public/test"},
	{description: "custom query script (GHSA-2x9q)", method: http.MethodGet, target: "/_QUERIES/queries/get_all", tableRead: true},
	{description: "custom query script, written by POST (GHSA-2x9q)", method: http.MethodPost, target: "/_QUERIES/rpt/d"},
	{description: "list registry (GHSA-2x9q)", method: http.MethodGet, target: "/_QUERIES/registry"},
	{description: "create registry entry (GHSA-2x9q)", method: http.MethodPost, target: "/_QUERIES/registry"},
	{description: "list registry for database (GHSA-2x9q)", method: http.MethodGet, target: "/_QUERIES/registry/prest-test", tableRead: true},
	{description: "get registry entry (GHSA-2x9q)", method: http.MethodGet, target: "/_QUERIES/registry/loc/name"},
	{description: "update registry entry (GHSA-2x9q)", method: http.MethodPut, target: "/_QUERIES/registry/loc/name"},
	{description: "delete registry entry (GHSA-2x9q)", method: http.MethodDelete, target: "/_QUERIES/registry/prest-test/loc/name"},
	{description: "the queries table, written through the table route (GHSA-2x9q)", method: http.MethodPost, target: "/prest/public/prest_queries"},
}

// Without a token, with auth on, none of them is served: a table read is
// refused by its stack before any handler runs, and the rest are not found.
func TestRegisterRoutes_AuthEnabled_AdvisoryRoutesAreNotServed(t *testing.T) {
	for _, tc := range advisoryTargets {
		t.Run(tc.description, func(t *testing.T) {
			f := newTestRouterWithRegistry(t, true, true)
			rec := serve(f, tc.method, tc.target, "")
			want := http.StatusNotFound
			if tc.tableRead {
				want = http.StatusUnauthorized
			}
			require.Equal(t, want, rec.Code, "body: %s", rec.Body.String())
		})
	}
}

// With auth off, as rest runs (PREST_AUTH_ENABLED=false), none is served: a
// table read names a Database rest was not given, and is not found.
func TestRegisterRoutes_AuthDisabled_AdvisoryRoutesAreNotServed(t *testing.T) {
	for _, tc := range advisoryTargets {
		t.Run(tc.description, func(t *testing.T) {
			f := newTestRouterWithRegistry(t, false, true)
			f.db.EXPECT().IsRegistered(gomock.Any()).Return(false).AnyTimes()
			f.adapter.EXPECT().TablePermissions(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(true).AnyTimes()
			rec := serve(f, tc.method, tc.target, "")
			require.Equal(t, http.StatusNotFound, rec.Code, "body: %s", rec.Body.String())
		})
	}
}

// The table read rest keeps sits behind the CRUD stack, so with auth on a
// request without a token is refused before any handler runs.
func TestRegisterRoutes_AuthEnabled_TheTableReadRejectsMissingToken(t *testing.T) {
	f := newTestRouter(t, true)
	rec := serve(f, http.MethodGet, "/prest-test/public/test", "")
	require.Equal(t, http.StatusUnauthorized, rec.Code, "body: %s", rec.Body.String())
}

// Liveness probes stay public even when auth is enabled, and touch no
// Database (patch 3): the pinger is given no expectation.
func TestRegisterRoutes_AuthEnabled_HealthIsPublic(t *testing.T) {
	f := newTestRouter(t, true)

	rec := serve(f, http.MethodGet, "/_health", "")
	require.NotEqual(t, http.StatusUnauthorized, rec.Code)
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
}
