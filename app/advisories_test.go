package app_test

// The three advisories pREST published on 19 October 2026, each asked of the
// composed stack (miniship-cloud#1452, #1453, #1454). Every subject that opens
// a connection counts it at the socket, with surface_test.go's
// countingDatabase, so "reached no Database" is a count and not a log line.
//
// rest never registered the routes the advisories are about (patch 4), so the
// requests they make are not found here before v2.5.2 and after it; those
// subjects hold that. What v2.5.2 itself fixes, and rest takes, is what a
// subject marked "fails before v2.5.2" proves: run against the fork at
// dab7a5d, the pin before this rebase, each of those is red.

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gorilla/mux"
	"github.com/stretchr/testify/require"

	"github.com/prest/prest/v2/adapters/postgres"
	"github.com/prest/prest/v2/app"
	"github.com/prest/prest/v2/config"
	"github.com/prest/prest/v2/plugins"
)

// GHSA-2x9q-r7j9-g85f: with queries.storage = "database", a stored query's SQL
// lives in public.prest_queries, the table routes could write it, and
// /_QUERIES ran it. restConf turns that storage on, so this is rest with the
// control plane configured as the advisory has it.

// The stored-query runner and its registry are not served, with the storage
// switched on.
func TestGHSA2x9q_theStoredQueryControlPlaneIsNotServed(t *testing.T) {
	db := newCountingDatabase(t)
	rest := newRest(t, db)

	for _, request := range []string{
		"POST /_QUERIES/rpt/d",
		"GET /_QUERIES/rpt/d",
		"POST /_QUERIES/given/rpt/d",
		"POST /_QUERIES/registry",
		"PUT /_QUERIES/registry/rpt/d",
		"POST /prest/public/prest_queries",
		"POST /given/public/prest_queries",
	} {
		rec := serve(rest, request)
		require.Equal(t, http.StatusNotFound, rec.Code, "%s: %s", request, rec.Body)
	}
	db.requireNoConnection(t)
}

// Fails before v2.5.2. pREST's own control-plane tables are reserved: the
// table read rest keeps refuses them before it reads, so the stored SQL cannot
// be read back through it either. Before v2.5.2 this read reached the
// Database.
func TestGHSA2x9q_theTableReadRefusesPRESTsControlPlaneTables(t *testing.T) {
	for _, table := range []string{"prest_queries", "prest_users", "PREST_QUERIES"} {
		t.Run(table, func(t *testing.T) {
			db := newCountingDatabase(t)
			rest := newRest(t, db)

			rec := serve(rest, "GET /given/public/"+table)
			require.Equal(t, http.StatusUnauthorized, rec.Code, rec.Body.String())
			db.requireNoConnection(t)
		})
	}
}

// Kept: the Supabase SDK's address is not pREST's, and a Project's own table
// may be called prest_users. Its read goes to the Database, whose grants
// decide, as any other table's does (patch 19); only pREST's reserved list is
// not consulted on it.
func TestGHSA2x9q_theSDKsAddressReadsAProjectsOwnTableOfThatName(t *testing.T) {
	db := newCountingDatabase(t)
	cfg := restConf(db)
	// As the cloud gateway hands rest its Project (patch 19).
	cfg.PostgREST.ProjectHeader = "X-Miniship-Project"
	cfg.Adapter = postgres.New(cfg)
	rest := newRestFrom(t, cfg)

	req := httptest.NewRequest(http.MethodGet, "/rest/v1/prest_users", nil)
	req.Header.Set("X-Miniship-Project", "given")
	rec := httptest.NewRecorder()
	rest.ServeHTTP(rec, req)
	require.NotEqual(t, http.StatusUnauthorized, rec.Code, rec.Body.String())
	db.requireAConnection(t)
}

// GHSA-fhf5-m388-4w4p: the catalog routes answered without a token, with
// pREST's auth on, and /databases listed every database on the host.

// The catalog is not served, with pREST's auth on or off, and asks nothing of
// the Database.
func TestGHSAfhf5_theCatalogIsNotServed(t *testing.T) {
	for _, authEnabled := range []bool{false, true} {
		db := newCountingDatabase(t)
		cfg := restConf(db)
		cfg.AuthEnabled = authEnabled
		cfg.JWTKey = "a-key-long-enough-for-hs256-signing"
		cfg.Adapter = postgres.New(cfg)
		rest := newRestFrom(t, cfg)

		for _, request := range []string{
			"GET /databases",
			"GET /schemas",
			"GET /tables",
			"GET /given/public",
			"GET /show/given/public/posts",
		} {
			rec := serve(rest, request)
			require.Equal(t, http.StatusNotFound, rec.Code, "auth %v, %s: %s", authEnabled, request, rec.Body)
		}
		db.requireNoConnection(t)
	}
}

// Fails before v2.5.2. Were the database listing ever asked, it is limited to
// the databases rest was given: the advisory's /databases read pg_database
// whole, every tenant on the host included.
func TestGHSAfhf5_theDatabaseListingIsLimitedToTheGivenDatabase(t *testing.T) {
	cfg := restConf(newCountingDatabase(t))
	adapter := postgres.New(cfg)
	lister, ok := adapter.(interface{ DatabaseWhere(string) string })
	require.True(t, ok)

	where := lister.DatabaseWhere("")
	require.Contains(t, where, "'given'", where)
	require.NotContains(t, lister.DatabaseWhere(""), "'postgres'")
}

// GHSA-fmr6-9j48-xrqp: /_PLUGIN/{file}/{func} joined {file} onto the plugin
// directory unchecked, and loaded whatever .so the traversal reached.

// The plugin route is not served: a GET of its shape is the table read, of a
// Database called _PLUGIN that rest was not given.
func TestGHSAfmr6_thePluginRouteIsNotServed(t *testing.T) {
	db := newCountingDatabase(t)
	rest := newRest(t, db)

	for _, request := range []string{
		"GET /_PLUGIN/hello/Hello",
		"POST /_PLUGIN/hello/Hello",
	} {
		rec := serve(rest, request)
		require.Equal(t, http.StatusNotFound, rec.Code, "%s: %s", request, rec.Body)
	}
	db.requireNoConnection(t)
}

// Fails before v2.5.2. The handler itself, which stays in the tree unrouted,
// refuses a name that leaves the plugin directory, before it opens anything,
// and says nothing of where the traversal pointed.
func TestGHSAfmr6_aPluginNameThatLeavesThePluginDirectoryIsRefusedUnopened(t *testing.T) {
	root := t.TempDir()
	pluginDir := filepath.Join(root, "plugins")
	outside := filepath.Join(root, "outside")
	require.NoError(t, os.MkdirAll(pluginDir, 0o755))
	require.NoError(t, os.MkdirAll(outside, 0o755))
	// Not a real plugin: opening it would fail. What is under test is that it
	// is never opened at all.
	require.NoError(t, os.WriteFile(filepath.Join(outside, "sample.so"), []byte("not a plugin"), 0o600))

	handler := plugins.New(&config.Prest{PluginPath: pluginDir}).Handler()
	for _, file := range []string{"../outside/sample", "../../" + strings.TrimPrefix(outside, "/") + "/sample", "/" + strings.TrimPrefix(outside, "/") + "/sample"} {
		req := mux.SetURLVars(httptest.NewRequest(http.MethodGet, "/_PLUGIN/x/someFunction", nil),
			map[string]string{"file": file, "func": "someFunction"})
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		require.Equal(t, http.StatusBadRequest, rec.Code, "%s: %s", file, rec.Body)
		require.Contains(t, rec.Body.String(), "invalid plugin name")
		require.NotContains(t, rec.Body.String(), "outside")
	}
}

// newRestFrom is newRest for a config the subject has already changed.
func newRestFrom(t *testing.T, cfg *config.Prest) http.Handler {
	t.Helper()
	a, err := app.New(cfg)
	require.NoError(t, err)
	return a.Handler
}
