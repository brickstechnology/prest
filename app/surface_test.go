package app_test

// rest's surface through the composed stack. pREST v2.4.2 registers 26 routes;
// rest answers two of them, and neither a removed route, nor the health
// address, nor a database rest was not given reaches a Database.
// router/surface_test.go walks the router itself.

import (
	"net"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/mux"
	"github.com/stretchr/testify/require"

	"github.com/prest/prest/v2/adapters/postgres"
	"github.com/prest/prest/v2/app"
	"github.com/prest/prest/v2/config"
	"github.com/prest/prest/v2/controllers"
	"github.com/prest/prest/v2/middlewares"
	"github.com/prest/prest/v2/plugins"
	"github.com/prest/prest/v2/router"
)

// upstreamRegistrations is every registration pREST v2.4.2 makes in
// router.RegisterRoutes with every optional route on, as its router reported
// them before the fork cut them, and a request of each shape. rest keeps two.
var upstreamRegistrations = []struct {
	registration string
	kept         bool
	requests     []string
}{
	{"POST /auth", false, []string{"POST /auth"}},
	{"GET,POST /_mcp", false, []string{"GET /_mcp", "POST /_mcp"}},
	{"GET /databases", false, []string{"GET /databases"}},
	{"GET /schemas", false, []string{"GET /schemas"}},
	{"GET /tables", false, []string{"GET /tables"}},
	{"GET /_QUERIES/registry", false, []string{"GET /_QUERIES/registry"}},
	{"POST /_QUERIES/registry", false, []string{"POST /_QUERIES/registry"}},
	{"GET /_QUERIES/registry/{location}/{name}", false, []string{"GET /_QUERIES/registry/queries/list"}},
	{"PUT /_QUERIES/registry/{location}/{name}", false, []string{"PUT /_QUERIES/registry/queries/list"}},
	{"DELETE /_QUERIES/registry/{location}/{name}", false, []string{"DELETE /_QUERIES/registry/queries/list"}},
	{"GET /_QUERIES/registry/{database}/{location}/{name}", false, []string{"GET /_QUERIES/registry/given/queries/list"}},
	{"PUT /_QUERIES/registry/{database}/{location}/{name}", false, []string{"PUT /_QUERIES/registry/given/queries/list"}},
	{"DELETE /_QUERIES/registry/{database}/{location}/{name}", false, []string{"DELETE /_QUERIES/registry/given/queries/list"}},
	{"ANY /_QUERIES/{queriesLocation}/{script}", false, []string{"GET /_QUERIES/queries/list", "POST /_QUERIES/queries/list"}},
	{"ANY /_QUERIES/{database}/{queriesLocation}/{script}", false, []string{"GET /_QUERIES/given/queries/list", "POST /_QUERIES/given/queries/list"}},
	{"ANY /_PLUGIN/{file}/{func}", false, []string{"GET /_PLUGIN/hello/Hello", "POST /_PLUGIN/hello/Hello"}},
	{"ANY /_studio", false, []string{"GET /_studio", "GET /_studio/", "GET /_studio/assets/index.js"}},
	{"GET /{database}/{schema}", false, []string{"GET /given/public"}},
	{"GET /show/{database}/{schema}/{table}", false, []string{"GET /show/given/public/posts"}},
	{"GET /_health", true, nil},
	{"GET /_ready", false, []string{"GET /_ready"}},
	{"GET /{database}/{schema}/{table}", true, nil},
	{"POST /{database}/{schema}/{table}", false, []string{"POST /given/public/posts"}},
	{"POST /batch/{database}/{schema}/{table}", false, []string{"POST /batch/given/public/posts"}},
	{"DELETE /{database}/{schema}/{table}", false, []string{"DELETE /given/public/posts"}},
	{"PUT,PATCH /{database}/{schema}/{table}", false, []string{"PUT /given/public/posts", "PATCH /given/public/posts"}},
}

func TestUpstreamRegistrations_are26AndRestKeepsTwo(t *testing.T) {
	require.Len(t, upstreamRegistrations, 26)
	var kept []string
	for _, u := range upstreamRegistrations {
		if u.kept {
			kept = append(kept, u.registration)
		}
	}
	sort.Strings(kept)
	require.Equal(t, []string{"GET /_health", "GET /{database}/{schema}/{table}"}, kept)
}

// countingDatabase stands where a Database would be: a TCP listener that
// counts the connections it accepts and closes each one without answering.
type countingDatabase struct {
	port    int
	accepts atomic.Int64
}

func newCountingDatabase(t *testing.T) *countingDatabase {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	db := &countingDatabase{port: ln.Addr().(*net.TCPAddr).Port}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			db.accepts.Add(1)
			_ = conn.Close()
		}
	}()
	t.Cleanup(func() { _ = ln.Close() })
	return db
}

// requireNoConnection waits long enough for a connection that was opened to
// have been counted, and fails if one was.
func (db *countingDatabase) requireNoConnection(t *testing.T) {
	t.Helper()
	require.Never(t, func() bool { return db.accepts.Load() > 0 },
		300*time.Millisecond, 10*time.Millisecond,
		"a connection reached the Database")
}

func (db *countingDatabase) requireAConnection(t *testing.T) {
	t.Helper()
	require.Eventually(t, func() bool { return db.accepts.Load() > 0 },
		10*time.Second, 10*time.Millisecond,
		"no connection reached the Database")
}

// restConf is a one-Database config without a registry, whose Database, and
// whose default host, is db. Every optional route upstream has that needs no
// start-up connection is on.
func restConf(db *countingDatabase) *config.Prest {
	return &config.Prest{
		HTTPTimeout:   5,
		PGHost:        "127.0.0.1",
		PGPort:        db.port,
		PGUser:        "authenticator",
		PGPass:        "not-a-real-password",
		PGDatabase:    "given",
		PGSSLMode:     "disable",
		PGConnTimeout: 2,
		PGMaxOpenConn: 1,
		JSONAggType:   "jsonb_agg",
		StudioConf:    config.StudioConf{Enabled: true},
		QueriesConf: config.QueriesConf{
			RegisterEnabled: true,
			Storage:         config.QueriesStorageDatabase,
		},
	}
}

// newRest is the composed stack as prestd builds it, with one Database: db.
func newRest(t *testing.T, db *countingDatabase) http.Handler {
	t.Helper()
	cfg := restConf(db)
	cfg.Adapter = postgres.New(cfg)
	a, err := app.New(cfg)
	require.NoError(t, err)
	return a.Handler
}

func serve(h http.Handler, request string) *httptest.ResponseRecorder {
	method, path, _ := strings.Cut(request, " ")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(method, path, strings.NewReader("{}")))
	return rec
}

// Not parallel, like every test here that opens connections: other tests in
// this package replace the connect function.
func TestRemovedRoutes_answer404AndReachNoDatabase(t *testing.T) {
	db := newCountingDatabase(t)
	rest := newRest(t, db)

	for _, u := range upstreamRegistrations {
		for _, request := range u.requests {
			rec := serve(rest, request)
			require.Equal(t, http.StatusNotFound, rec.Code,
				"%s (registered upstream as %s): %s", request, u.registration, rec.Body)
			require.Empty(t, rec.Header().Get("Allow"), request)
		}
	}
	db.requireNoConnection(t)
}

func TestHealth_answersWithoutTouchingADatabase(t *testing.T) {
	db := newCountingDatabase(t)
	rest := newRest(t, db)

	for range 5 {
		rec := serve(rest, "GET /_health")
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	}
	db.requireNoConnection(t)
}

// A name rest was not given is refused, and is never tried as a database name
// on the host rest does know.
func TestTableRead_refusesADatabaseItWasNotGiven(t *testing.T) {
	db := newCountingDatabase(t)
	rest := newRest(t, db)

	for _, name := range []string{"postgres", "template1", "other"} {
		rec := serve(rest, "GET /"+name+"/public/posts")
		require.Equal(t, http.StatusNotFound, rec.Code, "%s: %s", name, rec.Body)
		require.NotContains(t, rec.Body.String(), "127.0.0.1")
		require.NotContains(t, rec.Body.String(), strconv.Itoa(db.port))
	}
	db.requireNoConnection(t)

	// The control: the one name rest was given is tried on that same
	// listener, so the silence above is a refusal and not a miswired test.
	serve(rest, "GET /given/public/posts")
	db.requireAConnection(t)
}

// The same, with a registry of Databases. app.New connects to every registry
// entry as it starts, so this composes the handler as app.New does, without
// that start-up connection.
func TestTableRead_withARegistry_refusesADatabaseItWasNotGiven(t *testing.T) {
	db := newCountingDatabase(t)
	cfg := restConf(db)
	cfg.Databases = []config.DatabaseConf{{
		Alias:    "tenant-a",
		Host:     "127.0.0.1",
		Port:     db.port,
		User:     "authenticator",
		Database: "tenant_a",
		SSL:      config.DatabaseSSLConf{Mode: "disable"},
	}}
	cfg.Adapter = postgres.New(cfg)
	plg := plugins.New(cfg)
	r := mux.NewRouter().StrictSlash(true)
	router.RegisterRoutes(r, cfg, controllers.NewHandlersFromConfig(cfg),
		middlewares.NewCRUDStack(cfg, plg), nil, nil, plg)
	rest := middlewares.New(cfg)
	rest.UseHandler(r)

	// "given" is the default database name and "tenant_a" the registry
	// entry's physical name: neither is an alias rest was given.
	for _, name := range []string{"given", "tenant_a", "postgres", "other"} {
		rec := serve(rest, "GET /"+name+"/public/posts")
		require.Equal(t, http.StatusNotFound, rec.Code, "%s: %s", name, rec.Body)
		require.NotContains(t, rec.Body.String(), "127.0.0.1")
	}
	db.requireNoConnection(t)

	serve(rest, "GET /tenant-a/public/posts")
	db.requireAConnection(t)
}
