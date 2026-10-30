package controllers

// miniship (miniship-cloud#1890): a double-quoted name in a read's select,
// order, filters and write parameters, as PostgREST 14 reads it
// (QueryParams.hs, pFieldName and pQuotedValue), and the Database's own words
// in a failed read (Error.hs, v14.18). No Postgres is needed.

import (
	"errors"
	"net/http"
	"testing"

	"github.com/lib/pq"
	"github.com/stretchr/testify/require"
)

func TestPgrParseSelectList_aQuotedNameIsOneColumnWithTheSpacesOutsideDropped(t *testing.T) {
	// Q-02: select('name, "my col" ,d') is select=name%2C%22my+col%22%2Cd.
	items, err := pgrParseSelectList(`name, "my col" ,d`, 0)
	require.NoError(t, err)
	require.Len(t, items, 3)
	require.Equal(t, `"t"."name"`, items[0].field.sql("t"))
	require.Equal(t, `"t"."my col"`, items[1].field.sql("t"))
	require.Equal(t, `"t"."d"`, items[2].field.sql("t"))
}

func TestPgrParseSelectList_quotedNamesWhereverPostgRESTTakesAName(t *testing.T) {
	for raw, want := range map[string]string{
		// an alias, a column, both
		`"my alias":"my col"`: `"t"."my col" AS "my alias"`,
		`a:"my col"`:          `"t"."my col" AS "a"`,
		`"my alias":c`:        `"t"."c" AS "my alias"`,
		// a colon, a comma, a dot, a parenthesis and a double colon inside the quotes are text
		`"a:b"`:   `"t"."a:b"`,
		`"a,b"`:   `"t"."a,b"`,
		`"a.b"`:   `"t"."a.b"`,
		`"a(b)"`:  `"t"."a(b)"`,
		`"a::b"`:  `"t"."a::b"`,
		`"a->b"`:  `"t"."a->b"`,
		`"a!b"`:   `"t"."a!b"`,
		`"a\"b"`:  `"t"."a""b"`,
		`"a\\b"`:  `"t"."a\b"`,
		`"é ü"`:   `"t"."é ü"`,
		`"1 two"`: `"t"."1 two"`,
		// a cast
		`"my col"::text`: `CAST("t"."my col" AS text) AS "my col"`,
		// a json path: a quoted key is a key even when it reads as a number
		`"my col"->"a b"->>x`:  `("t"."my col"->'a b'->>'x') AS "x"`,
		`data->"0"`:            `("t"."data"->'0') AS "0"`,
		`data->0`:              `("t"."data"->0) AS "0"`,
		`data->"it's"->>"a.b"`: `("t"."data"->'it''s'->>'a.b') AS "a.b"`,
	} {
		items, err := pgrParseSelectList(raw, 0)
		require.NoError(t, err, raw)
		require.Len(t, items, 1, raw)
		require.Equal(t, want, items[0].field.sql("t"), raw)
	}
}

func TestPgrParseSelectList_aQuotedEmbed(t *testing.T) {
	items, err := pgrParseSelectList(`"my alias":"my rel"!"my hint"!inner(id,"a b")`, 0)
	require.NoError(t, err)
	require.Len(t, items, 1)
	embed := items[0].embed
	require.Equal(t, "my rel", embed.name)
	require.Equal(t, "my hint", embed.hint)
	require.Equal(t, "my alias", embed.label())
	require.True(t, embed.inner)
	require.Equal(t, `"t"."a b"`, embed.items[1].field.sql("t"))

	// the embedded table's own filters are addressed by its label, quoted or not
	found, rest := pgrFindEmbed(items, `"my alias".id`)
	require.Same(t, embed, found)
	require.Equal(t, "id", rest)
	found, rest = pgrFindEmbed(items, `my alias.order`)
	require.Same(t, embed, found)
	require.Equal(t, "order", rest)
}

func TestPgrQuotedNameNeverReachesSQLUnescaped(t *testing.T) {
	for _, c := range []struct{ raw, want string }{
		{`"x"; DROP TABLE users; --"`, ``}, // closes early: text follows the name
		{`"a\"; DROP TABLE users; --"`, `"t"."a""; DROP TABLE users; --"`},
		{`"a"" OR 1=1 --"`, ``}, // SQL's own doubling is not an escape here
		{`"a\\"; DROP TABLE users"`, ``},
		{`"a%s%d"`, `"t"."a%s%d"`},
		{`"$1"`, `"t"."$1"`},
	} {
		items, err := pgrParseSelectList(c.raw, 0)
		if c.want == "" {
			require.Error(t, err, c.raw)
			continue
		}
		require.NoError(t, err, c.raw)
		got := items[0].field.sql("t")
		require.Equal(t, c.want, got, c.raw)
	}

	// an unquoted name keeps the rule it had: letters, digits and _
	for _, raw := range []string{`a;b`, `a b`, `a"b`, `a"`, `"a`, `""`, `"a" b`, "\"a\x00b\"", `a->"b`, `a->"b" c`, `a->"b"x`} {
		_, err := pgrParseSelectList(raw, 0)
		require.Error(t, err, raw)
		var refused *pgrError
		require.ErrorAs(t, err, &refused, raw)
		require.Equal(t, "PGRST100", refused.code, raw)
	}
}

func TestPgrSplit_keepsTheQuotesAndSplitsOutsideThem(t *testing.T) {
	require.Equal(t, []string{`a`, ` "b,c" `, `d(e,f)`, `"g\",h"`}, pgrSplit(`a, "b,c" ,d(e,f),"g\",h"`))
	// a run that is never closed is one part, which pgrName then refuses
	require.Equal(t, []string{`"a,b`}, pgrSplit(`"a,b`))
}

func TestPgrParseOrder_aQuotedColumnAndItsDots(t *testing.T) {
	got, err := pgrParseOrder(`"my col".desc.nullslast,"a.b"->"c.d".asc,x`)
	require.NoError(t, err)
	require.Equal(t, ` ORDER BY "t"."my col" DESC NULLS LAST, ("t"."a.b"->'c.d') ASC, "t"."x" ASC`, got)

	_, err = pgrParseOrder(`"a"; DROP TABLE x.desc`)
	require.Error(t, err)
}

func TestPgrPredicate_aQuotedColumnInAFilterAndInAGroup(t *testing.T) {
	read := &pgrRead{}
	got, err := read.predicate(`"my col"`, "eq.1")
	require.NoError(t, err)
	require.Equal(t, `"t"."my col" = $1`, got)

	got, err = read.predicate(`"a.b"->>"c d"`, "eq.1")
	require.NoError(t, err)
	require.Equal(t, `("t"."a.b"->>'c d') = $2`, got)

	read = &pgrRead{}
	got, err = read.logicTree("or", `("my col".eq.1,"a,b".gt.2,"x(y)".is.null,d.eq."z")`)
	require.NoError(t, err)
	require.Equal(t, `("t"."my col" = $1 OR "t"."a,b" > $2 OR "t"."x(y)" IS NULL OR "t"."d" = $3)`, got)

	// a quote in the name is the name's, and the SQL doubles it
	read = &pgrRead{}
	got, err = read.logicTree("and", `("a\"b".eq.1)`)
	require.NoError(t, err)
	require.Equal(t, `("t"."a""b" = $1)`, got)

	read = &pgrRead{}
	_, err = read.logicTree("and", `("a".eq.1,"b)`)
	require.Error(t, err)
}

func TestPgrPredicate_isDistinctNullIsAValueNotTheKeyword(t *testing.T) {
	// F-16: PostgREST 14 reads everything after `isdistinct.` as an unknown
	// literal (QueryParams.hs:646, SqlFragment.hs:410), so `null` is the text
	// 'null', which a numeric column refuses as 22P02. Only `is` reads NULL.
	read := &pgrRead{}
	got, err := read.predicate("score", "isdistinct.null")
	require.NoError(t, err)
	require.Equal(t, `"t"."score" IS DISTINCT FROM $1`, got)
	require.Equal(t, []any{"null"}, read.args)

	read = &pgrRead{}
	got, err = read.predicate("score", "not.isdistinct.2")
	require.NoError(t, err)
	require.Equal(t, `NOT ("t"."score" IS DISTINCT FROM $1)`, got)
}

func TestPgrReadFailure_passesTheDatabasesWordsOn(t *testing.T) {
	for _, c := range []struct {
		name    string
		pq      pq.Error
		authed  bool
		status  int
		message string
	}{
		{"invalid text", pq.Error{Code: "22P02", Message: `invalid input syntax for type bigint: "abc"`}, true, http.StatusBadRequest, `invalid input syntax for type bigint: "abc"`},
		{"invalid date", pq.Error{Code: "22007", Message: `invalid input syntax for type date: "x"`}, true, http.StatusBadRequest, `invalid input syntax for type date: "x"`},
		{"invalid time", pq.Error{Code: "22008", Message: `date/time field value out of range: "99"`}, true, http.StatusBadRequest, `date/time field value out of range: "99"`},
		{"operator missing", pq.Error{Code: "42883", Message: `operator does not exist: integer ~~ unknown`, Hint: "No operator matches the given name and argument types. You might need to add explicit type casts."}, true, http.StatusNotFound, `operator does not exist: integer ~~ unknown`},
		{"no column", pq.Error{Code: "42703", Message: `column t.nope does not exist`}, true, http.StatusBadRequest, `column t.nope does not exist`},
		{"no privilege anonymous", pq.Error{Code: "42501", Message: `permission denied for table t`}, false, http.StatusUnauthorized, `permission denied for table t`},
		{"no privilege signed in", pq.Error{Code: "42501", Message: `permission denied for table t`}, true, http.StatusForbidden, `permission denied for table t`},
		{"geometry", pq.Error{Code: "22023", Message: "geometry column is missing"}, true, http.StatusBadRequest, "geometry column is missing"},
		{"raise", pq.Error{Code: "P0001", Message: "no", Detail: "because", Hint: "do not"}, true, http.StatusBadRequest, "no"},
		{"division", pq.Error{Code: "22012", Message: "division by zero"}, true, http.StatusBadRequest, "division by zero"},
	} {
		sent := c.pq
		got := pgrReadFailure(&sent, c.authed)
		var failure *pgrError
		require.True(t, errors.As(got, &failure), c.name)
		require.Equal(t, c.status, failure.status, c.name)
		require.Equal(t, string(c.pq.Code), failure.code, c.name)
		require.Equal(t, c.message, failure.message, c.name)
		require.Equal(t, c.pq.Detail, failure.details, c.name)
		require.Equal(t, c.pq.Hint, failure.hint, c.name)
	}
}

func TestPgrReadFailure_whatIsRestsOwnStaysRests(t *testing.T) {
	// the Database refusing rest's login says nothing of where it is
	got := pgrReadFailure(&pq.Error{Code: invalidPassword, Message: "password authentication failed for user \"x\""}, true)
	var failure *pgrError
	require.ErrorAs(t, got, &failure)
	require.Equal(t, http.StatusBadGateway, failure.status)
	require.Equal(t, "the Database could not be read", failure.message)
	require.Empty(t, failure.details)

	// a statement cancelled by its time limit is rest's 504
	got = pgrReadFailure(&pq.Error{Code: queryCanceled, Message: "canceling statement due to statement timeout"}, true)
	require.ErrorAs(t, got, &failure)
	require.Equal(t, http.StatusGatewayTimeout, failure.status)
	require.Equal(t, timeLimitMessage, failure.message)
}
