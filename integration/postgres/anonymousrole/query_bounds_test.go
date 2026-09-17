package anonymousrole_test

// miniship (#549): the read bounds and the error answers, against a live
// Postgres. controllers/query_screen_test.go and app/query_screen_test.go
// review the query string without one; these are the two claims a real
// database is the only place to make — a page that is capped rather than
// refused, and a statement the database itself cancels — and the error
// answers as they come back over the wire.
//
// Every subject here counts through needsPostgres, so it is in the census line
// TestMain prints and .github/workflows/miniship.yml checks for.

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/prest/prest/v2/config"
)

const (
	// many holds more rows than any ceiling a subject here sets.
	many = "rest_bounds_many"
	// slow is a view that sleeps in the database, so what cancels it is the
	// statement timeout and not anything in Go.
	slow = "rest_bounds_slow"
)

// stageBounds creates the two objects these subjects read and grants them to
// the reader. It is its own staging rather than an addition to stage(), so
// #546's subjects are unchanged by it.
func stageBounds(t *testing.T, db *sql.DB, rows int) {
	t.Helper()
	drop := fmt.Sprintf(`DROP VIEW IF EXISTS public.%s; DROP TABLE IF EXISTS public.%s;`, slow, many)
	exec(t, db, drop)
	t.Cleanup(func() { exec(t, db, drop) })
	exec(t, db, fmt.Sprintf(`
		CREATE TABLE public.%[1]s (id int PRIMARY KEY, title text);
		INSERT INTO public.%[1]s SELECT g, 'row ' || g FROM generate_series(1, %[3]d) g;
		GRANT SELECT ON public.%[1]s TO %[4]s;

		CREATE VIEW public.%[2]s AS SELECT 1 AS id, pg_sleep(5) IS NULL AS slept;
		GRANT SELECT ON public.%[2]s TO %[4]s;
	`, many, slow, rows, reader))
}

// rowsIn is how many records a 200 answer carried.
func rowsIn(t *testing.T, body string) int {
	t.Helper()
	var records []map[string]any
	require.NoError(t, json.Unmarshal([]byte(body), &records), body)
	return len(records)
}

// requireNoSecrets asserts an error answer says nothing about where the
// Database is, what rest logs in as, or what SQL it ran.
func requireNoSecrets(t *testing.T, cfg *config.Prest, body string) {
	t.Helper()
	for what, secret := range map[string]string{
		"the Database's host":   cfg.PGHost,
		"the Database's port":   strconv.Itoa(cfg.PGPort),
		"the login rest holds":  login,
		"the login's password":  password,
		"the connection string": "sslmode",
		"the driver":            "pq:",
	} {
		require.NotContains(t, body, secret, "the answer carries %s: %s", what, body)
	}
	for _, text := range []string{"SELECT", "jsonb_agg", "LIMIT", "OFFSET"} {
		require.NotContains(t, body, text, "the answer carries SQL text: %s", body)
	}
}

// A page larger than the ceiling is capped, not refused, and a read that asks
// for no page at all is given the ceiling — so there is no unbounded read.
func TestRest_aPageLargerThanTheCeilingIsCapped(t *testing.T) {
	cfg, db := needsPostgres(t)
	stageBounds(t, db, 300)
	rest := restAs(t, cfg, reader, func(c *config.Prest) { c.PGMaxPageSize = 10 })

	// Ask for a hundred times the ceiling.
	// Expected: 200 with the ceiling's worth of rows, not a refusal.
	status, body := get(t, rest, "/"+alias+"/public/"+many+"?_page=1&_page_size=1000")
	require.Equal(t, http.StatusOK, status, body)
	require.Equal(t, 10, rowsIn(t, body))

	// Ask for no page at all, which upstream answers with the whole table.
	// Expected: 200 with the ceiling's worth of rows.
	status, body = get(t, rest, "/"+alias+"/public/"+many)
	require.Equal(t, http.StatusOK, status, body)
	require.Equal(t, 10, rowsIn(t, body))

	// Ask for less than the ceiling.
	// Expected: what was asked for, so the ceiling is a ceiling and not a
	// fixed page.
	status, body = get(t, rest, "/"+alias+"/public/"+many+"?_page=2&_page_size=3")
	require.Equal(t, http.StatusOK, status, body)
	require.Equal(t, 3, rowsIn(t, body))

	// The count is the whole table, not the page.
	status, body = get(t, rest, "/"+alias+"/public/"+many+"?_count=*&_count_first=true")
	require.Equal(t, http.StatusOK, status, body)
	require.JSONEq(t, `{"count": 300}`, body)
}

// A statement that runs past the time limit is cancelled by the database
// itself, and the answer says so without saying anything else.
func TestRest_aStatementPastTheTimeLimitIsCancelled(t *testing.T) {
	cfg, db := needsPostgres(t)
	stageBounds(t, db, 5)
	rest := restAs(t, cfg, reader, func(c *config.Prest) { c.PGStatementTimeoutMS = 250 })

	// Read the view that sleeps for five seconds, under a limit of 250ms.
	// Expected: 504 well inside the sleep, with a message a caller can act on.
	started := time.Now()
	status, body := get(t, rest, "/"+alias+"/public/"+slow)
	require.Equal(t, http.StatusGatewayTimeout, status, body)
	require.Less(t, time.Since(started), 4*time.Second, "the statement was not cancelled, it finished")
	require.Contains(t, body, "time limit")
	require.NotContains(t, body, "true")
	requireNoSecrets(t, cfg, body)

	// The control: a read that finishes inside the limit still answers, on the
	// same server, so the limit is a limit and not a broken connection.
	status, body = get(t, rest, "/"+alias+"/public/"+many)
	require.Equal(t, http.StatusOK, status, body)
	require.Equal(t, 5, rowsIn(t, body))
}

// The time limit is the whole transaction's, not one connection's: the pooled
// connection goes back without it, so the next read is not cancelled by the
// last one's limit.
func TestRest_theTimeLimitDoesNotOutliveItsTransaction(t *testing.T) {
	cfg, db := needsPostgres(t)
	stageBounds(t, db, 5)
	rest := restAs(t, cfg, reader, func(c *config.Prest) { c.PGStatementTimeoutMS = 250 })

	for range 3 {
		status, body := get(t, rest, "/"+alias+"/public/"+slow)
		require.Equal(t, http.StatusGatewayTimeout, status, body)
		status, body = get(t, rest, "/"+alias+"/public/"+many)
		require.Equal(t, http.StatusOK, status, body)
		require.Equal(t, 5, rowsIn(t, body))
	}
}

// Every error answer a live Database can produce, and what it may say.
func TestRest_noErrorAnswerCarriesASecret(t *testing.T) {
	cfg, db := needsPostgres(t)
	stageBounds(t, db, 5)
	rest := restAs(t, cfg, reader)

	for _, c := range []struct {
		path   string
		status int
	}{
		// A table that is not there.
		{"/" + alias + "/public/rest_bounds_absent", http.StatusNotFound},
		// A column that is not there, in each parameter that names one.
		{"/" + alias + "/public/" + many + "?_order=absent", http.StatusBadRequest},
		{"/" + alias + "/public/" + many + "?_select=absent", http.StatusBadRequest},
		{"/" + alias + "/public/" + many + "?absent=$eq.1", http.StatusBadRequest},
		// A table the anonymous role holds no grant on.
		{"/" + alias + "/public/rest_anonrole_ungranted", http.StatusForbidden},
		// A Database rest was not given, and a schema it does not serve.
		{"/not-a-project/public/" + many, http.StatusNotFound},
		{"/" + alias + "/" + private + "/posts", http.StatusNotFound},
		// The query string, refused, in each of its three shapes.
		{"/" + alias + "/public/" + many + "?_join=inner:pg_catalog.pg_class:public." + many + ".id:$eq:pg_class.oid", http.StatusBadRequest},
		{"/" + alias + "/public/" + many + "?_not_a_parameter=1", http.StatusBadRequest},
		{"/" + alias + "/public/" + many + "?title=$ltreematch%20x", http.StatusBadRequest},
	} {
		status, body := get(t, rest, c.path)
		require.Equal(t, c.status, status, "%s: %s", c.path, body)
		requireNoSecrets(t, cfg, body)
	}

	// The role that could not be entered, which is the one 500 a live Database
	// can produce. #546 proves it serves nothing; this proves it says nothing.
	stranded := restAs(t, cfg, stranger)
	status, body := get(t, stranded, "/"+alias+"/public/"+many)
	require.Equal(t, http.StatusInternalServerError, status, body)
	requireNoSecrets(t, cfg, body)
	require.NotContains(t, body, stranger)
}

// What the anonymous role could read outside public, rest refuses itself. The
// role here is granted the catalog and the other schema; rest still answers
// nothing.
func TestRest_refusesWhatReachesOutsidePublicEvenWhenTheRoleMay(t *testing.T) {
	cfg, db := needsPostgres(t)
	stageBounds(t, db, 5)
	rest := restAs(t, cfg, reader)

	// A join into the catalog, a join into the other schema, and a
	// schema-qualified name in each parameter that takes one.
	for _, query := range []string{
		"_join=inner:pg_catalog.pg_class:public." + many + ".id:$eq:pg_class.oid",
		"_join=inner:" + private + ".posts:public." + many + ".id:$eq:posts.id",
		"_select=pg_catalog.pg_class.relname",
		"_select=" + private + ".posts.title",
		"_order=" + private + ".posts.id",
		private + ".posts.id=$eq.1",
		"_select=current_user",
		"_select=version()",
	} {
		status, body := get(t, rest, "/"+alias+"/public/"+many+"?"+query)
		require.Equal(t, http.StatusBadRequest, status, "%s: %s", query, body)
		require.NotContains(t, body, "a row outside public", query)
		require.NotContains(t, body, reader, query)
		requireNoSecrets(t, cfg, body)
	}

	// The control: the reader really can read both of those in the database,
	// so the refusals above are rest's and not the role's.
	var outside, catalog int
	require.NoError(t, db.QueryRow(fmt.Sprintf(
		`SELECT count(*) FROM %s.posts`, private)).Scan(&outside))
	require.Positive(t, outside)
	require.NoError(t, db.QueryRow(
		`SELECT count(*) FROM pg_catalog.pg_class WHERE relname = $1`, many).Scan(&catalog))
	require.Positive(t, catalog)
}
