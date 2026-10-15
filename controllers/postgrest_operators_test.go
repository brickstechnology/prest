package controllers

// miniship (miniship-cloud#1427): how the read parses the array, range, jsonb
// and full-text operators and splits a group around their values, with no
// Database. The rows they return are held by
// integration/postgres/anonymousrole/postgrest_operators_test.go.

import (
	"net/url"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPgrSplitGroupKeepsABraceRunAndAQuotedRange(t *testing.T) {
	for in, want := range map[string][]string{
		`a.eq.1,b.eq.2`:                        {`a.eq.1`, `b.eq.2`},
		`tags.cs.{a,b},tags.cs.{d}`:            {`tags.cs.{a,b}`, `tags.cs.{d}`},
		`meta.cs.{"k":"v","n":1},x.is.null`:    {`meta.cs.{"k":"v","n":1}`, `x.is.null`},
		`span.sl."[15,30)",span.adj."[10,12)"`: {`span.sl.[15,30)`, `span.adj.[10,12)`},
		`and(a.cs.{x,y},b.eq."p,q"),c.eq.3`:    {`and(a.cs.{x,y},b.eq."p,q")`, `c.eq.3`},
		`not.or(a.eq.1,b.cs.{1,2}),c.eq."a,b"`: {`not.or(a.eq.1,b.cs.{1,2})`, `c.eq.a,b`},
	} {
		require.Equal(t, want, pgrSplitGroup(in), in)
	}
}

func TestPgrPredicateWritesTheOperatorsAsPostgRESTDoes(t *testing.T) {
	for _, c := range []struct{ column, expr, sql string }{
		{"tags", "cs.{a,b}", `"t"."tags" @> $1`},
		{"tags", "cd.{a,b}", `"t"."tags" <@ $1`},
		{"tags", "ov.{a,b}", `"t"."tags" && $1`},
		{"span", "sl.[1,2)", `"t"."span" << $1`},
		{"span", "sr.[1,2)", `"t"."span" >> $1`},
		{"span", "nxl.[1,2)", `"t"."span" &> $1`},
		{"span", "nxr.[1,2)", `"t"."span" &< $1`},
		{"span", "adj.[1,2)", `"t"."span" -|- $1`},
		{"span", "not.adj.[1,2)", `NOT ("t"."span" -|- $1)`},
		{"tsv", "fts.fox", `"t"."tsv" @@ to_tsquery($1)`},
		{"tsv", "fts(english).fox", `"t"."tsv" @@ to_tsquery($1, $2)`},
		{"tsv", "plfts(english).fox", `"t"."tsv" @@ plainto_tsquery($1, $2)`},
		{"tsv", "phfts(english).fox", `"t"."tsv" @@ phraseto_tsquery($1, $2)`},
		{"tsv", "wfts(english).fox", `"t"."tsv" @@ websearch_to_tsquery($1, $2)`},
		{"tsv", "not.wfts.fox", `NOT ("t"."tsv" @@ websearch_to_tsquery($1))`},
	} {
		read := &pgrRead{}
		got, err := read.predicate(c.column, c.expr)
		require.NoError(t, err, c.expr)
		require.Equal(t, c.sql, got, c.expr)
	}

	// The configuration is the first argument and the value the second.
	read := &pgrRead{}
	_, err := read.predicate("tsv", "fts(english).quick & fox")
	require.NoError(t, err)
	require.Equal(t, []any{"english", "quick & fox"}, read.args)

	// A value is a parameter, never SQL text, and keeps its dots.
	read = &pgrRead{}
	got, err := read.predicate("meta", `cs.{"a.b":"x'); DROP TABLE t; --"}`)
	require.NoError(t, err)
	require.Equal(t, `"t"."meta" @> $1`, got)
	require.Equal(t, []any{`{"a.b":"x'); DROP TABLE t; --"}`}, read.args)
}

func TestPgrPredicateRefusesWhatPostgRESTsParserRefuses(t *testing.T) {
	for _, expr := range []string{"fts().x", "fts(english.x", "fts(en;glish).x", "overlaps.{a}", "cs", "nope.x"} {
		_, err := (&pgrRead{}).predicate("tsv", expr)
		var refused *pgrError
		require.ErrorAs(t, err, &refused, expr)
		require.Equal(t, "PGRST100", refused.code, expr)
	}
}

func TestPgrParseReadTakesTheOperatorsInAnOrGroup(t *testing.T) {
	query := url.Values{
		"select": {"id"},
		"or":     {`(tags.cs.{a,b},meta.cs.{"k":"v","n":1},span.adj."[1,2)")`},
	}
	read, err := pgrParseRead(query, 100)
	require.NoError(t, err)
	require.Equal(t, []string{`("t"."tags" @> $1 OR "t"."meta" @> $2 OR "t"."span" -|- $3)`}, read.where)
	require.Equal(t, []any{"{a,b}", `{"k":"v","n":1}`, "[1,2)"}, read.args)
}
