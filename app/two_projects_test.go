package app_test

// One rest serves two Projects, and one nobody calls costs nothing
// (miniship-cloud#547).
//
// Two properties, asked of the composed stack with a registry of three
// Databases. Both are counted at the socket, by the listener
// surface_test.go's countingDatabase already is: a connection either reached a
// Database or it did not, and no log line or pool statistic is consulted.
//
//   - Start-up reaches no Database. Upstream's app.New connected to and pinged
//     every registry entry before the first caller existed, and probed
//     TimescaleDB when there was no registry. At 250 Projects that woke 250
//     computes against a ceiling of 20 concurrently active, so the registry
//     spent the whole budget on nobody.
//   - A read reaches the Database its path names, and no other. Upstream's
//     AdapterSelectorMiddleware ran outside the router, so mux.Vars was empty
//     and it never saw {database}; every handler then used whichever adapter
//     the registry's map handed back first.
//
// integration/postgres/twoprojects asks a live Postgres the same two
// questions, with real rows, real backends and a measured idle window.

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/prest/prest/v2/app"
	"github.com/prest/prest/v2/config"
)

// The three Projects rest is given. Only the second is ever called.
const (
	projectA = "alpha"
	projectB = "beta"
	projectC = "gamma"
)

// restProjectsConf is rest as the cloud composes it: a registry of Databases,
// each on its own host, each read as its own anonymous role, with pREST's
// single-database mode off and its access list off.
func restProjectsConf(dbs map[string]*countingDatabase) *config.Prest {
	cfg := &config.Prest{
		HTTPTimeout:   5,
		PGUser:        "authenticator",
		PGPass:        "not-a-real-password",
		PGSSLMode:     "disable",
		PGConnTimeout: 2,
		PGMaxOpenConn: 1,
		JSONAggType:   "jsonb_agg",
		SingleDB:      false,
		AccessConf:    config.AccessConf{Restrict: false},
	}
	for _, alias := range []string{projectA, projectB, projectC} {
		cfg.Databases = append(cfg.Databases, config.DatabaseConf{
			Alias:       alias,
			Host:        "127.0.0.1",
			Port:        dbs[alias].port,
			User:        "authenticator",
			Pass:        "not-a-real-password",
			Database:    alias,
			SSL:         config.DatabaseSSLConf{Mode: "disable"},
			MaxOpenConn: 1,
			AnonRole:    "app_anon",
		})
	}
	return cfg
}

// newRestWithProjects composes rest over three Databases and returns it with
// the listener standing where each one is.
func newRestWithProjects(t *testing.T) (*app.App, map[string]*countingDatabase) {
	t.Helper()
	dbs := map[string]*countingDatabase{
		projectA: newCountingDatabase(t),
		projectB: newCountingDatabase(t),
		projectC: newCountingDatabase(t),
	}
	a, err := app.New(restProjectsConf(dbs))
	require.NoError(t, err, "composing rest must not need a Database to answer")
	require.True(t, a.Adapters.IsRegistered(projectA))
	require.True(t, a.Adapters.IsRegistered(projectB))
	require.True(t, a.Adapters.IsRegistered(projectC))
	return a, dbs
}

// Not parallel, like every test in this package that opens connections.
func TestStartUp_reachesNoDatabase(t *testing.T) {
	rest, dbs := newRestWithProjects(t)

	// And a health check does not reach one either, however often it is asked.
	for range 5 {
		rec := serve(rest.Handler, "GET /_health")
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	}

	for alias, db := range dbs {
		t.Run(alias, func(t *testing.T) { db.requireNoConnection(t) })
	}
}

func TestTableRead_reachesOnlyTheProjectItsPathNames(t *testing.T) {
	rest, dbs := newRestWithProjects(t)

	// Read the second Project's table. The listener standing where it is
	// answers nothing, so the read fails — what is under test is which
	// Database the connection was opened to.
	serve(rest.Handler, "GET /"+projectB+"/public/posts")
	dbs[projectB].requireAConnection(t)

	// The other two are never named by a path, so nothing reaches them: not
	// the Project registered before the one called, and not the one after it.
	dbs[projectA].requireNoConnection(t)
	dbs[projectC].requireNoConnection(t)
}

// Each adapter was given one Database and can resolve no other, so a read that
// reached the wrong one is refused rather than quietly answered from the right
// connection: the tenant boundary is the adapter's, not only the router's.
func TestEachProject_hasAnAdapterThatCarriesItAlone(t *testing.T) {
	rest, _ := newRestWithProjects(t)

	for _, mine := range []string{projectA, projectB, projectC} {
		adapter, err := rest.Adapters.Get(mine)
		require.NoError(t, err)
		require.True(t, adapter.IsRegistered(mine), mine)
		for _, theirs := range []string{projectA, projectB, projectC} {
			if theirs == mine {
				continue
			}
			require.False(t, adapter.IsRegistered(theirs),
				"%s's adapter resolves %s", mine, theirs)
		}
	}
}

// A name rest was not given is refused before a connection, with a registry
// as without one: it is not tried as a database of that name on any registered
// Project's host.
func TestTableRead_refusesANameNoProjectCarries(t *testing.T) {
	rest, dbs := newRestWithProjects(t)

	for _, name := range []string{"postgres", "template1", "delta"} {
		rec := serve(rest.Handler, "GET /"+name+"/public/posts")
		require.Equal(t, http.StatusNotFound, rec.Code, "%s: %s", name, rec.Body)
	}
	for alias, db := range dbs {
		t.Run(alias, func(t *testing.T) { db.requireNoConnection(t) })
	}
}
