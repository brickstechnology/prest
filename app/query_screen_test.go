package app_test

// miniship (#549): the query string of a table read, through the composed
// stack. Every request here is refused, and the counting Database proves the
// refusal happened before any SQL ran: not one connection was opened.
//
// controllers/query_screen_test.go is the same review at the screen itself,
// parameter by parameter. docs/miniship/query-string.md is the review.

import (
	"net/http"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"
)

// reachesOutsidePublic is every shape that names something outside the served
// schema. rest refuses each itself, before any SQL: the anonymous role would
// refuse most of them too, and that is not enough.
var reachesOutsidePublic = []string{
	// A join, which upstream accepts schema-qualified: this one reads a
	// catalog every role may read.
	"_join=inner:pg_catalog.pg_class:public.posts.id:$eq:pg_class.oid",
	"_join=inner:information_schema.tables:public.posts.id:$eq:tables.table_name",
	// A schema-qualified name, in each parameter that takes a name.
	"_select=pg_catalog.pg_authid",
	"_order=pg_catalog.pg_authid",
	"_groupby=pg_catalog.pg_authid",
	"_count=pg_catalog.pg_authid",
	"pg_catalog.pg_authid=$eq.1",
	// A function call.
	"_select=pg_read_file('/etc/passwd')",
	"_groupby=pg_sleep(1)",
	"_select=version()",
}

func TestTableRead_refusesWhatReachesOutsidePublic(t *testing.T) {
	db := newCountingDatabase(t)
	rest := newRest(t, db)

	for _, query := range reachesOutsidePublic {
		rec := serve(rest, "GET /given/public/posts?"+query)
		require.Equal(t, http.StatusBadRequest, rec.Code, "%s: %s", query, rec.Body)
		require.NotContains(t, rec.Body.String(), "127.0.0.1", query)
		require.NotContains(t, rec.Body.String(), strconv.Itoa(db.port), query)
	}
	db.requireNoConnection(t)

	// The control: the same read without any of them reaches the Database, so
	// the silence above is a refusal and not a miswired test.
	serve(rest, "GET /given/public/posts")
	db.requireAConnection(t)
}
