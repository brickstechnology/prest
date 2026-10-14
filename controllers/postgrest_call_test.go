package controllers

// miniship (miniship-cloud#1304): the parts of a function call that need no
// Database: which query parameters are arguments, which overload a call means,
// the words of PGRST202 and PGRST203, and the statement the call becomes.

import (
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPgrSplitCallQuery_aValueThatIsAnOperatorIsAFilterAndAnyOtherIsAnArgument(t *testing.T) {
	query := url.Values{
		"min_id":  {"1"},
		"id":      {"gt.1", "5"},
		"name":    {"not.eq.x"},
		"tags":    {"{a,b}"},
		"order":   {"id.desc"},
		"limit":   {"2"},
		"select":  {"id"},
		"or":      {"(id.eq.1)"},
		"b.limit": {"1"},
		"note":    {"eq"}, // no dot after the operator: an argument
		"fts":     {"fts(english).word"},
	}
	shaped, arguments := pgrSplitCallQuery(query)

	require.Equal(t, url.Values{
		"id": {"gt.1"}, "name": {"not.eq.x"}, "order": {"id.desc"}, "limit": {"2"}, "select": {"id"},
		"or": {"(id.eq.1)"}, "b.limit": {"1"}, "fts": {"fts(english).word"},
	}, shaped)
	require.Equal(t, []pgrCallArgument{{"id", "5"}, {"min_id", "1"}, {"note", "eq"}, {"tags", "{a,b}"}}, arguments)
}

func routine(result pgrResult, params ...pgrParam) pgrRoutine {
	return pgrRoutine{name: "f", params: params, result: result}
}

func param(name, typ string, required bool) pgrParam {
	return pgrParam{name: name, typ: typ, required: required}
}

func keys(names ...string) map[string]bool {
	out := map[string]bool{}
	for _, n := range names {
		out[n] = true
	}
	return out
}

func TestPgrFindRoutine_theArgumentNamesDecideNotTheirTypes(t *testing.T) {
	cases := []struct {
		name     string
		routines []pgrRoutine
		sent     map[string]bool
		single   bool
		post     bool
		want     int // index of the routine, -1 for none, -2 for ambiguous
	}{
		{"no argument, none sent", []pgrRoutine{routine(pgrResultScalar)}, keys(), false, true, 0},
		{"no argument, one sent", []pgrRoutine{routine(pgrResultScalar)}, keys("a"), false, true, -1},
		{"every required argument", []pgrRoutine{routine(pgrResultScalar, param("a", "integer", true), param("b", "integer", true))}, keys("a", "b"), false, false, 0},
		{"a required argument missing", []pgrRoutine{routine(pgrResultScalar, param("a", "integer", true), param("b", "integer", true))}, keys("a"), false, false, -1},
		{"an argument it does not take", []pgrRoutine{routine(pgrResultScalar, param("a", "integer", true))}, keys("a", "z"), false, false, -1},
		{"only optional arguments, none sent", []pgrRoutine{routine(pgrResultScalar, param("a", "text", false))}, keys(), false, false, 0},
		{"only optional arguments, one of them", []pgrRoutine{routine(pgrResultScalar, param("a", "text", false), param("b", "text", false))}, keys("b"), false, false, 0},
		{"required and optional, the required sent", []pgrRoutine{routine(pgrResultScalar, param("a", "text", true), param("b", "text", false))}, keys("a"), false, false, 0},
		{"required and optional, both sent", []pgrRoutine{routine(pgrResultScalar, param("a", "text", true), param("b", "text", false))}, keys("a", "b"), false, false, 0},
		{"required and optional, the required missing", []pgrRoutine{routine(pgrResultScalar, param("a", "text", true), param("b", "text", false))}, keys("b"), false, false, -1},
		{
			"overloads told apart by their names",
			[]pgrRoutine{routine(pgrResultScalar, param("a", "integer", true)), routine(pgrResultScalar, param("a", "integer", true), param("b", "integer", true))},
			keys("a", "b"), false, true, 1,
		},
		{
			"overloads told apart only by their types",
			[]pgrRoutine{routine(pgrResultScalar, param("a", "bigint", true)), routine(pgrResultScalar, param("a", "integer", true))},
			keys("a"), false, true, -2,
		},
		{"single-object takes one json argument", []pgrRoutine{routine(pgrResultScalar, param("j", "jsonb", true))}, keys("x", "y"), true, true, 0},
		{"single-object does not take two", []pgrRoutine{routine(pgrResultScalar, param("a", "integer", true), param("b", "integer", true))}, keys("a", "b"), true, true, -1},
		{"single-object does not take an integer", []pgrRoutine{routine(pgrResultScalar, param("a", "integer", true))}, keys("a"), true, true, -1},
		{"a POST falls back to the one json argument", []pgrRoutine{routine(pgrResultScalar, param("j", "json", true))}, keys("anything"), false, true, 0},
		{"a GET does not", []pgrRoutine{routine(pgrResultScalar, param("j", "json", true))}, keys("anything"), false, false, -1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, candidates, err := pgrFindRoutine("f", c.routines, c.sent, c.single, c.post)
			switch c.want {
			case -1:
				require.ErrorIs(t, err, errNoRoutine)
			case -2:
				require.ErrorIs(t, err, errAmbiguousRoutine)
				require.Len(t, candidates, 2)
			default:
				require.NoError(t, err)
				require.Equal(t, c.routines[c.want].params, got.params)
			}
		})
	}
}

func TestPgrNoRoutine_saysWhatPostgRESTSays(t *testing.T) {
	overloads := []pgrRoutine{routine(pgrResultScalar, param("val", "integer", true), param("name", "text", true), param("param", "text", true))}
	failure := pgrNoRoutine("test", []string{"pqaram", "vall", "nam"}, false, true, overloads, nil).(*pgrError)
	require.Equal(t, "PGRST202", failure.code)
	require.Equal(t, 404, failure.status)
	require.Equal(t, "Could not find the function public.test(nam, pqaram, vall) in the schema cache", failure.message)
	require.Equal(t, "Searched for the function public.test with parameters nam, pqaram, vall or with a single unnamed json/jsonb parameter, but no matches were found in the schema cache.", failure.details)
	require.Equal(t, "Perhaps you meant to call the function public.test(name, param, val)", failure.hint)

	// One parameter is singular; none is "without parameters"; a GET says
	// nothing of a single unnamed parameter.
	failure = pgrNoRoutine("test", []string{"x"}, false, false, overloads, nil).(*pgrError)
	require.Equal(t, "Searched for the function public.test with parameter x, but no matches were found in the schema cache.", failure.details)
	failure = pgrNoRoutine("nothing", nil, false, false, nil, []string{"test"}).(*pgrError)
	require.Equal(t, "Could not find the function public.nothing without parameters in the schema cache", failure.message)
	require.Empty(t, failure.hint)

	// A name that is nearly another function's.
	failure = pgrNoRoutine("testt", []string{"a"}, false, true, nil, []string{"test", "another"}).(*pgrError)
	require.Equal(t, "Perhaps you meant to call the function public.test", failure.hint)

	// With params=single-object the message names no arguments and there is no hint.
	failure = pgrNoRoutine("test", []string{"a"}, true, true, overloads, nil).(*pgrError)
	require.Equal(t, "Could not find the function public.test in the schema cache", failure.message)
	require.Equal(t, "Searched for the function public.test with a single json/jsonb parameter, but no matches were found in the schema cache.", failure.details)
	require.Empty(t, failure.hint)
}

func TestPgrAmbiguousRoutine_listsTheCandidates(t *testing.T) {
	failure := pgrAmbiguousRoutine([]pgrRoutine{
		{name: "f", params: []pgrParam{param("a", "bigint", true)}},
		{name: "f", params: []pgrParam{param("a", "integer", true), param("b", "text", true)}},
	}).(*pgrError)
	require.Equal(t, 300, failure.status)
	require.Equal(t, "PGRST203", failure.code)
	require.Equal(t, "Could not choose the best candidate function between: public.f(a => bigint), public.f(a => integer, b => text)", failure.message)
	require.Equal(t, "Try renaming the parameters or the function itself in the database so function overloading can be resolved", failure.hint)
}

func TestPgrParseCallBody(t *testing.T) {
	body, err := pgrParseCallBody([]byte(` {"b": 1, "a": [1,2]} `))
	require.NoError(t, err)
	require.True(t, body.isObject)
	require.Equal(t, keys("a", "b"), body.keys)

	// An empty body is {}, as PostgREST reads it.
	body, err = pgrParseCallBody(nil)
	require.NoError(t, err)
	require.Equal(t, "{}", string(body.raw))
	require.Empty(t, body.keys)

	// An array names the keys of its first object; anything else, none.
	body, err = pgrParseCallBody([]byte(`[{"a":1},{"a":2}]`))
	require.NoError(t, err)
	require.False(t, body.isObject)
	require.Equal(t, keys("a"), body.keys)
	body, err = pgrParseCallBody([]byte(`42`))
	require.NoError(t, err)
	require.Empty(t, body.keys)

	_, err = pgrParseCallBody([]byte(`{"a":`))
	require.Equal(t, "PGRST102", err.(*pgrError).code)
}

func TestPgrCallPlan_statementIsOneStatementThatNamesTheArgumentsAndQuotesThem(t *testing.T) {
	read, err := pgrParseRead(url.Values{"id": {"gt.1"}, "order": {"id.desc"}, "limit": {"2"}}, 5000)
	require.NoError(t, err)

	table := &pgrRoutine{name: "titles", result: pgrResultSetOfComposite,
		params: []pgrParam{param("min_id", "integer", true), {name: "weird \"name\"", typ: "text", required: false}}}
	plan := pgrPlanCall(table, read, keys("min_id", `weird "name"`), false, `{"min_id":1}`)
	statement, args, err := plan.statement(true)
	require.NoError(t, err)
	require.Equal(t, `WITH "pgrst_source" AS (SELECT "pgrst_call".* FROM json_to_record($2::json) AS "pgrst_body"("min_id" integer, "weird ""name""" text), LATERAL "public"."titles"("min_id" := "pgrst_body"."min_id", "weird ""name""" := "pgrst_body"."weird ""name""") AS "pgrst_call") `+
		`SELECT (SELECT coalesce(json_agg("x"), '[]'::json)::text FROM (SELECT "t".* FROM "pgrst_source" AS "t" WHERE "t"."id" > $1 ORDER BY "t"."id" DESC LIMIT 2 OFFSET 0) AS "x"), `+
		`(SELECT count(*) FROM "pgrst_source" AS "t" WHERE "t"."id" > $1)`, statement)
	require.Equal(t, []any{"1", `{"min_id":1}`}, args)

	// A scalar is read from the one column the call is wrapped to have, and a
	// function of no arguments sends no JSON.
	scalar := &pgrRoutine{name: "hello", result: pgrResultScalar}
	plain, err := pgrParseRead(url.Values{}, 5000)
	require.NoError(t, err)
	statement, args, err = pgrPlanCall(scalar, plain, keys(), false, "").statement(false)
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(statement, `WITH "pgrst_source" AS (SELECT "pgrst_call"."pgrst_scalar" FROM (SELECT "public"."hello"() AS "pgrst_scalar") AS "pgrst_call") `), statement)
	require.Contains(t, statement, `json_agg("x"."pgrst_scalar")`)
	require.True(t, strings.HasSuffix(statement, `, NULL::bigint`), statement)
	require.Empty(t, args)

	// A function of one json argument is given the whole body, cast to its type.
	one := &pgrRoutine{name: "echo", result: pgrResultScalar, params: []pgrParam{param("j", "jsonb", true)}}
	statement, args, err = pgrPlanCall(one, plain, keys("x"), true, `{"x":1}`).statement(false)
	require.NoError(t, err)
	require.Contains(t, statement, `"public"."echo"($1::jsonb)`)
	require.Equal(t, []any{`{"x":1}`}, args)

	// A variadic argument is passed as VARIADIC.
	many := &pgrRoutine{name: "sum", result: pgrResultScalar, params: []pgrParam{{name: "nums", typ: "integer[]", required: true, variadic: true}}}
	statement, _, err = pgrPlanCall(many, plain, keys("nums"), false, `{"nums":[1]}`).statement(false)
	require.NoError(t, err)
	require.Contains(t, statement, `"public"."sum"(VARIADIC "nums" := "pgrst_body"."nums")`)
}

func TestPgrQueryArguments_theLastValueWinsAndAVariadicOneKeepsEvery(t *testing.T) {
	sent := []pgrCallArgument{{"a", "1"}, {"a", "2"}, {"nums", "4"}, {"nums", "5"}}
	plain := &pgrRoutine{params: []pgrParam{param("a", "integer", true)}}
	require.JSONEq(t, `{"a":"2","nums":"5"}`, pgrQueryArguments(plain, sent))
	variadic := &pgrRoutine{params: []pgrParam{param("a", "integer", true), {name: "nums", typ: "integer[]", variadic: true}}}
	require.JSONEq(t, `{"a":"2","nums":["4","5"]}`, pgrQueryArguments(variadic, sent))
}

func TestPgrDatabaseStatus_aFunctionMayRaiseItsOwnStatus(t *testing.T) {
	require.Equal(t, 418, pgrDatabaseStatus("PT418", "a teapot", true))
	require.Equal(t, 500, pgrDatabaseStatus("PT9", "not a status", true))
	require.Equal(t, 400, pgrDatabaseStatus("P0001", "raised", true))
	require.Equal(t, 405, pgrDatabaseStatus("25006", "cannot execute INSERT in a read-only transaction", true))
}
