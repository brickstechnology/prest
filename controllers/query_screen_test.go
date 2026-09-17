package controllers

// miniship (#549): the review of rest's query string, pinned.
//
// docs/miniship/query-string.md is the review in prose: every parameter, kept,
// restricted or removed, and why. This file is the same list as tests, so the
// document and the code cannot drift apart without one of these failing.
//
// Every refusal here is proved to happen *before any SQL runs*: the handler is
// built with a recordingReader, and a refused request leaves it with nothing
// recorded. app/query_screen_test.go makes the same proof one level out, with
// a Database that counts the connections it is given.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/prest/prest/v2/adapters/postgres"
)

// queryValues reads a raw query string of this file's own literals. It is not
// url.ParseQuery: these literals carry the bytes an attacker would send —
// semicolons, quotes, per-cent signs — and ParseQuery refuses some of them
// before the screen ever sees them, which is the wrong test.
func queryValues(q string) url.Values {
	values := url.Values{}
	if q == "" {
		return values
	}
	for _, pair := range strings.Split(q, "&") {
		key, value, _ := strings.Cut(pair, "=")
		values.Add(key, value)
	}
	return values
}

// screenOf is the screened query string of q, or the refusal.
func screenOf(t *testing.T, q string) (url.Values, error) {
	t.Helper()
	return screenTableRead(queryValues(q), QueryBounds{})
}

// refusedBeforeAnySQL asserts that a read carrying query is refused with 400,
// that nothing was read, and that the answer quotes none of the request back.
func refusedBeforeAnySQL(t *testing.T, query string) {
	t.Helper()
	reader := &recordingReader{answer: answering(`[{"title":"a row the caller must not get"}]`, nil)}
	h := anonymousRead(t, staticRoles{"prest-test": "app_anon"}, reader, "public")

	rec := selectPosts(h, "public", "?"+queryValues(query).Encode())
	require.Equal(t, http.StatusBadRequest, rec.Code, "%s: %s", query, rec.Body)
	require.NotContains(t, rec.Body.String(), "a row the caller must not get", query)
	require.Empty(t, reader.recorded(), "%s reached the Database", query)
}

// servedBeforeAnySQL asserts that a read carrying query is served, so that a
// refusal above is the screen's judgement and not a test that refuses
// everything.
func servedBeforeAnySQL(t *testing.T, query string) {
	t.Helper()
	reader := &recordingReader{answer: answering(`[{"title":"a published post"}]`, nil)}
	h := anonymousRead(t, staticRoles{"prest-test": "app_anon"}, reader, "public")

	rec := selectPosts(h, "public", "?"+queryValues(query).Encode())
	require.Equal(t, http.StatusOK, rec.Code, "%s: %s", query, rec.Body)
	require.Len(t, reader.recorded(), 1, query)
}

// ── The advisory ────────────────────────────────────────────────────────────

// ghsaInputs are GHSA-qvx3-q8vx-9q3c's own values, from the advisory's PoC and
// from the two sinks its Details section names: SelectFields, reached with
// _select alone, and CountByRequest, reached with _select beside _count. The
// shortcut it exploited — any field containing a double-quoted run is
// concatenated raw — is gone from upstream since v2.3.0; these are here so the
// fork proves it for itself, on its own tree, in its own suite.
var ghsaInputs = []string{
	// The PoC: a role's password hash out of the system catalog.
	`_select=(SELECT rolpassword FROM pg_authid WHERE rolname='postgres' LIMIT 1)"h"`,
	// The same value at the second sink, CountByRequest.
	`_count=*&_select=(SELECT rolpassword FROM pg_authid WHERE rolname='postgres' LIMIT 1)"h"`,
	// The advisory's other two named payloads: a host file, and the blind
	// oracle it says a sub-select allows.
	`_select=(SELECT pg_read_file('/etc/passwd'))"f"`,
	`_select=(SELECT pg_sleep(5))"s"`,
	// GHSA-rhfr-5rf5-f2h5, the same shortcut used to redirect the FROM.
	`_select="id" FROM "public"."secret") s --`,
	// GHSA-vm7c-w4p2-6cp9, the CountByRequest sink on its own.
	`_count=*&_select=1 FROM pg_shadow--`,
}

func TestGHSA_qvx3_q8vx_9q3c_isRefusedBeforeAnySQL(t *testing.T) {
	t.Parallel()
	for _, query := range ghsaInputs {
		refusedBeforeAnySQL(t, query)
	}

	// The control from the advisory itself: "A normal _select=id,title behaves
	// correctly, proving the injection is the double-quoted field, not the
	// endpoint."
	servedBeforeAnySQL(t, "_select=id,title")
}

// ── The review, parameter by parameter ──────────────────────────────────────

// parameter is one row of docs/miniship/query-string.md.
type parameter struct {
	name string
	// served is a request rest answers, for a kept parameter.
	served []string
	// refused is a request rest refuses. For a removed parameter these are
	// every spelling of it; for a kept one they are the escapes tried out of
	// it.
	refused []string
}

// kept is every parameter the review keeps, with the escapes tried out of each:
// into SQL through quoting, comments, stacked statements or identifier tricks;
// into another schema; into pg_catalog; into another Database.
var kept = []parameter{{
	name:   "_select",
	served: []string{"_select=id", "_select=id,title", "_select=*", "_select=sum:votes", "_select=SUM:votes:total"},
	refused: []string{
		`_select=id"`,                           // quoting
		`_select=id';SELECT 1`,                  // a stacked statement
		"_select=id--",                          // a comment
		"_select=id/*x*/",                       // a block comment
		"_select=public.posts",                  // an identifier trick: a qualified name
		"_select=private.secret",                // another schema
		"_select=pg_catalog.pg_authid",          // the catalog
		"_select=other_project.public.posts",    // another Database
		"_select=pg_read_file('/etc/passwd')",   // a function call
		"_select=count:id",                      // a function outside the six
		"_select=sum:id:alias:extra",            // the aggregate spelling, overrun
		`_select=SUM("id") AS "x", pg_sleep(1)`, // the shape sanitizeSelectField quotes
	},
}, {
	name:   "_count",
	served: []string{"_count=*", "_count=id"},
	refused: []string{
		"_count=*)--",
		"_count=id;SELECT 1",
		"_count=pg_catalog.pg_authid",
		"_count=private.secret",
		"_count=other_project.public.posts",
		"_count=count(*)",
		"_count=id,title",    // COUNT takes one argument
		"_count=*&_count=id", // given twice
	},
}, {
	name:   "_count_first",
	served: []string{"_count=*&_count_first=true"},
	refused: []string{
		"_count=*&_count_first=1", "_count=*&_count_first=yes", "_count=*&_count_first=",
		"_count=*&_count_first=true;SELECT 1",
		"_count=*&_count_first=pg_catalog.pg_authid",
	},
}, {
	name:   "_distinct",
	served: []string{"_distinct=true", "_distinct=false"},
	refused: []string{
		"_distinct=TRUE", "_distinct=true--", "_distinct=1",
		"_distinct=true) UNION SELECT 1 FROM pg_authid--",
		"_distinct=private.secret",
	},
}, {
	name:   "_order",
	served: []string{"_order=id", "_order=-id", "_order=id,-title"},
	refused: []string{
		"_order=id;SELECT 1",
		"_order=id--",
		`_order=id"`,
		"_order=public.posts.id",
		"_order=private.secret",
		"_order=pg_catalog.pg_authid",
		"_order=other_project.public.posts",
		"_order=(SELECT 1)",
		"_order=id DESC", // upstream spells descending with a leading -
	},
}, {
	name:   "_groupby",
	served: []string{"_groupby=title", "_groupby=title,id"},
	refused: []string{
		"_groupby=title->>having:sum:votes:$gt:10", // the having sub-grammar
		"_groupby=time_bucket('1 minute',ts)",      // an allowlisted function call
		"_groupby=pg_sleep(1)",
		"_groupby=title;SELECT 1",
		"_groupby=private.secret",
		"_groupby=pg_catalog.pg_authid",
		"_groupby=other_project.public.posts",
		`_groupby=title"`,
	},
}, {
	name:   "_page",
	served: []string{"_page=1", "_page=2&_page_size=10"},
	refused: []string{
		"_page=0",
		"_page=-1",
		"_page=1;SELECT 1",
		"_page=one",
		"_page=1&_page=2",
		"_page=1 OFFSET 0",
		"_page=pg_catalog.pg_authid",
	},
}, {
	name:   "_page_size",
	served: []string{"_page=1&_page_size=1", "_page=1&_page_size=100"},
	refused: []string{
		"_page_size=0",
		"_page_size=ten",
		"_page_size=10--",
		"_page_size=10;SELECT 1",
		"_page_size=1&_page_size=2",
		"_page_size=private.secret",
	},
}, {
	name: "a column filter",
	served: []string{
		"title=hello",
		"title=$eq.hello",
		"votes=$gt.10",
		"id=$in.1,2,3",
		"title=$null.",
		"title=$like.%a%",
		// The value is bound as a parameter, so SQL inside it is a string and
		// not a statement.
		"title=' OR 1=1 --",
		"title=$eq.'; DROP TABLE posts; --",
		"body->>author:jsonb=$eq.ana",
	},
	refused: []string{
		"public.posts.id=$eq.1",             // a qualified name
		"pg_catalog.pg_authid=$eq.1",        // the catalog
		"private.secret=$eq.1",              // another schema
		"other_project.public.posts=$eq.1",  // another Database
		`id"=$eq.1`,                         // quoting
		"id--=$eq.1",                        // a comment
		"id;SELECT 1=$eq.1",                 // a stacked statement
		"id=$ltreematch.*.a",                // an operator the review removed
		"id=$nosuchop.1",                    // an operator that does not exist
		"title=x $eq. y",                    // an operator found mid-value
		"title=$eq.a$ne.b",                  // a second operator, which upstream also strips
		"body->>author->>x:jsonb=$eq.ana",   // a jsonb key that is not a name
		"body->>'author':jsonb=$eq.ana",     // quoting, in the jsonb key
		"private.body->>author:jsonb=$eq.a", // another schema, in the jsonb column
		"title:tsquery=x",                   // full-text search, removed
		"embedding:vecdist=l2:lt:[1,2]:0.5", // pgvector, removed
		"title:nosuchtype=x",
	},
}}

// upstream's own operator regex ends in an unescaped `.`, so it reads any byte
// after the name — a space included — and GetQueryOperator then strips the `$`
// and the spaces. A screen that looked for `$op.` literally would see no
// operator in any of these and wave the value through to a builder that then
// applied one. These are that hole, closed.
var operatorsSpeltTheOtherWay = []string{
	"id=$ltreematch x",     // the ltree match operator: a caller's POSIX regex
	"id=$ltreelanc x",      //
	"id=$ltreerdesc x",     //
	"id=$ltreematchtxt x",  //
	"id=abc$ltreematch x",  // and not at the front of the value either
	"id=$nosuchoperator x", //
}

func TestScreen_anOperatorSpeltTheWayUpstreamReadsItIsRefused(t *testing.T) {
	t.Parallel()
	for _, query := range operatorsSpeltTheOtherWay {
		refusedBeforeAnySQL(t, query)
	}
}

// removed is every parameter the review removes, and the spellings of it that
// must now be refused.
var removed = []parameter{{
	name: "_join",
	refused: []string{
		"_join=inner:public.comments:public.posts.id:$eq:comments.post_id",
		"_join=inner:pg_catalog.pg_class:public.posts.id:$eq:pg_class.oid",
		"_join=left:private.secret:public.posts.id:$eq:secret.id",
	},
}, {
	name: "_or",
	refused: []string{
		"_or=title=$eq.a||title=$eq.b",
		"_or=title=$eq.a or title=$eq.b",
	},
}, {
	name:    "_korder",
	refused: []string{"_korder=embedding:l2:[1,2,3]"},
}, {
	name:    "_renderer",
	refused: []string{"_renderer=xml", "_renderer=json"},
}, {
	name:    "_time_bucket",
	refused: []string{"_time_bucket=ts:1 minute"},
}, {
	name:    "_returning",
	refused: []string{"_returning=id", "_returning=*"},
}, {
	name:    "_param",
	refused: []string{"_param=x"},
}, {
	name:    "_header",
	refused: []string{"_header=x"},
}, {
	name:    "_include_system_schemas",
	refused: []string{"_include_system_schemas=true"},
}, {
	name:    "anything upstream grows next",
	refused: []string{"_not_a_parameter=1", "_=1", "_select_=id"},
}}

func TestScreen_everyKeptParameterServesItsOwnShapeAndRefusesTheEscapes(t *testing.T) {
	t.Parallel()
	for _, p := range kept {
		t.Run(p.name, func(t *testing.T) {
			t.Parallel()
			for _, query := range p.served {
				servedBeforeAnySQL(t, query)
			}
			for _, query := range p.refused {
				refusedBeforeAnySQL(t, query)
			}
		})
	}
}

func TestScreen_everyRemovedParameterIsRefusedBeforeAnySQL(t *testing.T) {
	t.Parallel()
	for _, p := range removed {
		t.Run(p.name, func(t *testing.T) {
			t.Parallel()
			for _, query := range p.refused {
				refusedBeforeAnySQL(t, query)
			}
		})
	}
}

// The review is the list: a parameter that is neither kept nor removed here is
// one nobody judged, and the screen's own switch is what would serve it.
func TestScreen_theReviewNamesEveryParameterTheTreeReads(t *testing.T) {
	t.Parallel()
	judged := map[string]bool{}
	for _, p := range append(append([]parameter{}, kept...), removed...) {
		judged[p.name] = true
	}
	for _, name := range []string{
		"_select", "_count", "_count_first", "_distinct", "_order", "_groupby",
		"_page", "_page_size", "a column filter",
		"_join", "_or", "_korder", "_renderer", "_time_bucket", "_returning",
		"_param", "_header", "_include_system_schemas",
	} {
		require.True(t, judged[name], "%s is in the tree and not in the review", name)
	}
}

// An unknown parameter is answered with what rest does serve, and that list is
// the switch itself: a parameter added to one and not the other would make the
// answer a lie.
func TestScreen_theAnswerNamesWhatIsServed(t *testing.T) {
	t.Parallel()
	_, err := screenOf(t, "_not_a_parameter=1")
	require.ErrorIs(t, err, ErrQueryNotAccepted)
	for _, name := range servedParameters {
		require.Contains(t, err.Error(), name)
		_, err := screenOf(t, name+"=") // whatever it answers, it is not "unknown"
		if err != nil {
			require.NotContains(t, err.Error(), "not a parameter rest serves", name)
		}
	}
	// And nothing of the caller's is quoted back.
	_, err = screenOf(t, "_haxx0r_<script>=1")
	require.NotContains(t, err.Error(), "haxx0r")
}

// _select reaches two sinks, and CountByRequest reads only the first value of
// the parameter. The screen hands over one comma-separated value so the count
// and the projection are built from the same columns.
func TestScreen_selectIsOneValueSoTheCountSeesEveryColumn(t *testing.T) {
	t.Parallel()
	screened, err := screenOf(t, "_count=*&_select=id,title")
	require.NoError(t, err)
	require.Equal(t, []string{"id,title"}, screened["_select"])
	require.Equal(t, "id,title", screened.Get("_select"))

	// Given twice, it is still one value.
	screened, err = screenOf(t, "_select=id&_select=title")
	require.NoError(t, err)
	require.Equal(t, []string{"id,title"}, screened["_select"])
}

// What the builders read is the screen's spelling, not the caller's.
func TestScreen_handsOverItsOwnSpelling(t *testing.T) {
	t.Parallel()
	screened, err := screenOf(t, "_select= id , title &_groupby= title &_order= -id ")
	require.NoError(t, err)
	require.Equal(t, "id,title", screened.Get("_select"))
	require.Equal(t, "title", screened.Get("_groupby"))
	require.Equal(t, "-id", screened.Get("_order"))
}

// ── The bounds ──────────────────────────────────────────────────────────────

func TestScreen_boundsEveryRead(t *testing.T) {
	t.Parallel()

	// A read that asks for no page at all.
	// Expected: the ceiling, so the answer cannot be the whole table.
	screened, err := screenOf(t, "")
	require.NoError(t, err)
	require.Equal(t, "1", screened.Get("_page"))
	require.Equal(t, strconv.Itoa(defaultMaxPageSize), screened.Get("_page_size"))

	// A page larger than the ceiling.
	// Expected: capped, not refused.
	screened, err = screenOf(t, "_page=3&_page_size="+strconv.Itoa(defaultMaxPageSize*4))
	require.NoError(t, err)
	require.Equal(t, "3", screened.Get("_page"))
	require.Equal(t, strconv.Itoa(defaultMaxPageSize), screened.Get("_page_size"))

	// A page under the ceiling.
	// Expected: the caller's own, untouched.
	screened, err = screenOf(t, "_page=2&_page_size=25")
	require.NoError(t, err)
	require.Equal(t, "25", screened.Get("_page_size"))

	// A ceiling an operator set.
	screened, err = screenTableRead(url.Values{"_page_size": {"900"}}, QueryBounds{MaxPageSize: 100})
	require.NoError(t, err)
	require.Equal(t, "100", screened.Get("_page_size"))
}

func TestScreen_theCeilingsAreTheResearchsOwnNumbers(t *testing.T) {
	t.Parallel()
	// docs/research/QUERY-BOUNDS-AND-FAIRNESS.md §1.1: Loki's
	// max_entries_limit_per_query, 5000 log lines returned per query — the one
	// rows-returned limit in that survey with a real, non-zero default. §1.3:
	// VictoriaLogs' -search.maxQueryLen, 16KB, the one query-complexity bound
	// in it expressed as a hard byte cap.
	require.Equal(t, 5000, defaultMaxPageSize)
	require.Equal(t, 16384, defaultMaxQueryLen)
	// And a handler built with no bounds at all still carries both, so a
	// caller cannot reach an unbounded read through a miswired composition.
	require.Equal(t, 5000, QueryBounds{}.withDefaults().MaxPageSize)
	require.Equal(t, 16384, QueryBounds{}.withDefaults().MaxQueryLen)
}

// A query string longer than the bound is refused before it is parsed: every
// filter in it is a predicate built and a parameter bound, which is work the
// page ceiling and the time limit are both downstream of.
func TestTableRead_aQueryStringLongerThanTheBoundIsRefused(t *testing.T) {
	t.Parallel()
	reader := &recordingReader{answer: answering(`[{"title":"a row the caller must not get"}]`, nil)}
	h := anonymousRead(t, staticRoles{"prest-test": "app_anon"}, reader, "public")

	filters := make([]string, 0, 4000)
	for i := range 4000 {
		filters = append(filters, fmt.Sprintf("column_%d=$eq.%d", i, i))
	}
	long := strings.Join(filters, "&")
	require.Greater(t, len(long), defaultMaxQueryLen)

	rec := selectPosts(h, "public", "?"+long)
	require.Equal(t, http.StatusRequestURITooLong, rec.Code, rec.Body.String())
	require.Empty(t, reader.recorded())

	// The control: the same shape, inside the bound, is read.
	rec = selectPosts(h, "public", "?"+strings.Join(filters[:10], "&"))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Len(t, reader.recorded(), 1)
}

// The request deadline and the statement's own limit are the same answer: a
// caller can do the same thing about either.
func TestTableRead_aRequestPastItsDeadlineIsAnswered504(t *testing.T) {
	t.Parallel()
	reader := &recordingReader{answer: answering("", context.DeadlineExceeded)}
	h := anonymousRead(t, staticRoles{"prest-test": "app_anon"}, reader, "public")

	rec := selectPosts(h, "public", "")
	require.Equal(t, http.StatusGatewayTimeout, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), "time limit")
	require.NotContains(t, rec.Body.String(), "context deadline exceeded")
}

// A fault of rest's own is not reported as the Database's.
func TestTableRead_aFaultOfRestsOwnIsNotTheDatabases(t *testing.T) {
	t.Parallel()
	reader := &recordingReader{answer: answering("", errors.New("json: unsupported value"))}
	h := anonymousRead(t, staticRoles{"prest-test": "app_anon"}, reader, "public")

	rec := selectPosts(h, "public", "")
	require.Equal(t, http.StatusInternalServerError, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), "the read could not be completed")
	require.NotContains(t, rec.Body.String(), "json: unsupported value")
}

// ── The screen is an allowlist by construction ──────────────────────────────

func TestScreen_handsOnWhatItPutThereAndNothingElse(t *testing.T) {
	t.Parallel()
	screened, err := screenOf(t, "_select=id&title=$eq.a&_order=-id&_page=2&_page_size=5")
	require.NoError(t, err)

	names := make([]string, 0, len(screened))
	for name := range screened {
		names = append(names, name)
	}
	require.ElementsMatch(t, []string{"_select", "title", "_order", "_page", "_page_size"}, names)

	// The one value that is not the caller's own is the page it was given, and
	// the screen wrote it.
	require.Equal(t, "id", screened.Get("_select"))
	require.Equal(t, "$eq.a", screened.Get("title"))
}

// The screen keeps its own operator list rather than reading upstream's, so an
// operator upstream adds is refused here until somebody reviews it. This holds
// that list to being a subset of upstream's, so every operator the screen keeps
// still resolves to SQL.
func TestScreenOperators_areASubsetOfUpstreams(t *testing.T) {
	t.Parallel()
	for op := range filterOperators {
		sql, err := postgres.GetQueryOperator("$" + op)
		require.NoError(t, err, op)
		require.NotEmpty(t, sql, op)
	}
	// And the four upstream operators the review removed are still upstream's,
	// so this is a removal and not a rename.
	for _, op := range []string{"$ltreelanc", "$ltreerdesc", "$ltreematch", "$ltreematchtxt"} {
		_, err := postgres.GetQueryOperator(op)
		require.NoError(t, err, op)
		require.NotContains(t, filterOperators, strings.TrimPrefix(op, "$"))
	}
}
