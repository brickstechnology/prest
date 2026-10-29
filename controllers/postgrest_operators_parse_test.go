package controllers

// miniship (miniship-cloud#1887): how the read parses the whole of PostgREST
// 14's filter language, with no Database: every operator, the (any) and (all)
// forms, not., in lists, is values, and/or trees, and the refusals with the
// message and details PostgREST gives. The rows are held by
// integration/postgres/anonymousrole/postgrest_operators_test.go.

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPgrPredicateWritesEveryOperatorForm(t *testing.T) {
	for _, c := range []struct {
		expr, sql string
		args      []any
	}{
		{"eq.5", `"t"."c" = $1`, []any{"5"}},
		{"gte.5", `"t"."c" >= $1`, []any{"5"}},
		{"gt.5", `"t"."c" > $1`, []any{"5"}},
		{"lte.5", `"t"."c" <= $1`, []any{"5"}},
		{"lt.5", `"t"."c" < $1`, []any{"5"}},
		{"neq.5", `"t"."c" <> $1`, []any{"5"}},
		{"like.a*", `"t"."c" LIKE $1`, []any{"a%"}},
		{"ilike.a*b", `"t"."c" ILIKE $1`, []any{"a%b"}},
		{"match.^x*", `"t"."c" ~ $1`, []any{"^x*"}},
		{"imatch.^x", `"t"."c" ~* $1`, []any{"^x"}},
		{"isdistinct.5", `"t"."c" IS DISTINCT FROM $1`, []any{"5"}},
		{"not.isdistinct.5", `NOT ("t"."c" IS DISTINCT FROM $1)`, []any{"5"}},
		{"eq(any).{1,2}", `"t"."c" = ANY($1)`, []any{"{1,2}"}},
		{"eq(all).{1,2}", `"t"."c" = ALL($1)`, []any{"{1,2}"}},
		{"gt(any).{1,2}", `"t"."c" > ANY($1)`, []any{"{1,2}"}},
		{"gte(all).{1,2}", `"t"."c" >= ALL($1)`, []any{"{1,2}"}},
		{"lt(any).{1,2}", `"t"."c" < ANY($1)`, []any{"{1,2}"}},
		{"lte(all).{1,2}", `"t"."c" <= ALL($1)`, []any{"{1,2}"}},
		{`like(all).{"%a%","%n%"}`, `"t"."c" LIKE ALL($1)`, []any{`{"%a%","%n%"}`}},
		{"like(any).{Ja*,La*}", `"t"."c" LIKE ANY($1)`, []any{"{Ja%,La%}"}},
		{"ilike(any).{ja%,la%}", `"t"."c" ILIKE ANY($1)`, []any{"{ja%,la%}"}},
		{"ilike(all).{%a%}", `"t"."c" ILIKE ALL($1)`, []any{"{%a%}"}},
		{"match(any).{^a,b$}", `"t"."c" ~ ANY($1)`, []any{"{^a,b$}"}},
		{"imatch(all).{^a,b$}", `"t"."c" ~* ALL($1)`, []any{"{^a,b$}"}},
		{"not.like(all).{a,b}", `NOT ("t"."c" LIKE ALL($1))`, []any{"{a,b}"}},
		{"is.null", `"t"."c" IS NULL`, nil},
		{"is.NULL", `"t"."c" IS NULL`, nil},
		{"is.not_null", `"t"."c" IS NOT NULL`, nil},
		{"is.Not_Null", `"t"."c" IS NOT NULL`, nil},
		{"is.true", `"t"."c" IS TRUE`, nil},
		{"is.FALSE", `"t"."c" IS FALSE`, nil},
		{"is.unknown", `"t"."c" IS UNKNOWN`, nil},
		{"not.is.null", `NOT ("t"."c" IS NULL)`, nil},
		{"in.(1,2,3)", `"t"."c" = ANY($1)`, []any{`{"1","2","3"}`}},
		{`in.(1,"a,b",c)`, `"t"."c" = ANY($1)`, []any{`{"1","a,b","c"}`}},
		{`in.("a (b)","c)")`, `"t"."c" = ANY($1)`, []any{`{"a (b)","c)"}`}},
		{`in.("a\"b","c\\d")`, `"t"."c" = ANY($1)`, []any{`{"a\"b","c\\d"}`}},
		{"in.()", `"t"."c" = ANY('{}')`, nil},
		{"not.in.()", `NOT ("t"."c" = ANY('{}'))`, nil},
		{`not.in.(x,"y,z")`, `NOT ("t"."c" = ANY($1))`, []any{`{"x","y,z"}`}},
		{`in.( 1,2 )`, `"t"."c" = ANY($1)`, []any{`{"1","2 "}`}},
	} {
		read := &pgrRead{}
		got, err := read.predicate("c", c.expr)
		require.NoError(t, err, c.expr)
		require.Equal(t, c.sql, got, c.expr)
		if c.args == nil {
			require.Empty(t, read.args, c.expr)
		} else {
			require.Equal(t, c.args, read.args, c.expr)
		}
	}
}

// A value is only ever a parameter: an injection in a list element is one
// string of the array literal.
func TestPgrPredicateKeepsAnInjectionInAListAParameter(t *testing.T) {
	read := &pgrRead{}
	got, err := read.predicate("c", `in.("x'); DROP TABLE t; --",y)`)
	require.NoError(t, err)
	require.Equal(t, `"t"."c" = ANY($1)`, got)
	require.Equal(t, []any{`{"x'); DROP TABLE t; --","y"}`}, read.args)

	read = &pgrRead{}
	got, err = read.predicate("c", `like(any).{"x') OR 1=1 --"}`)
	require.NoError(t, err)
	require.Equal(t, `"t"."c" LIKE ANY($1)`, got)
	require.NotContains(t, got, "OR 1=1")
}

func TestPgrLogicTreeReadsEveryOperatorInAGroup(t *testing.T) {
	for _, c := range []struct {
		op, value, sql string
		args           []any
	}{
		{"or", `(a.eq.1,b.gt.2)`, `("t"."a" = $1 OR "t"."b" > $2)`, []any{"1", "2"}},
		{"and", `(a.eq.1,b.gt.2)`, `("t"."a" = $1 AND "t"."b" > $2)`, []any{"1", "2"}},
		{"not.or", `(a.eq.1,b.eq.2)`, `NOT ("t"."a" = $1 OR "t"."b" = $2)`, []any{"1", "2"}},
		{"not.and", `(a.eq.1,b.eq.2)`, `NOT ("t"."a" = $1 AND "t"."b" = $2)`, []any{"1", "2"}},
		{"or", `(a.eq.1,and(b.eq.2,c.eq.3))`, `("t"."a" = $1 OR ("t"."b" = $2 AND "t"."c" = $3))`, []any{"1", "2", "3"}},
		{"or", `(a.eq.1,not.and(b.eq.2,or(c.eq.3,d.eq.4)))`,
			`("t"."a" = $1 OR NOT ("t"."b" = $2 AND ("t"."c" = $3 OR "t"."d" = $4)))`, []any{"1", "2", "3", "4"}},
		{"or", `(a.not.eq.1,b.not.in.(1,2))`, `(NOT ("t"."a" = $1) OR NOT ("t"."b" = ANY($2)))`, []any{"1", `{"1","2"}`}},
		{"or", `(a.match.^x,b.imatch.^y,c.isdistinct.z)`, `("t"."a" ~ $1 OR "t"."b" ~* $2 OR "t"."c" IS DISTINCT FROM $3)`, []any{"^x", "^y", "z"}},
		{"or", `(a.like(all).{"%a%","%n%"},b.eq(any).{1,2})`, `("t"."a" LIKE ALL($1) OR "t"."b" = ANY($2))`, []any{`{"%a%","%n%"}`, "{1,2}"}},
		{"or", `(a.like(any).{Ja*,La*},b.ilike(all).{x})`, `("t"."a" LIKE ANY($1) OR "t"."b" ILIKE ALL($2))`, []any{"{Ja%,La%}", "{x}"}},
		{"and", `(a.in.("x,y",z),b.is.null)`, `("t"."a" = ANY($1) AND "t"."b" IS NULL)`, []any{`{"x,y","z"}`}},
		{"or", `(a.is.not_null,b.is.unknown,c.in.())`, `("t"."a" IS NOT NULL OR "t"."b" IS UNKNOWN OR "t"."c" = ANY('{}'))`, nil},
		{"or", `(a.eq."p,q",b.eq."say \"hi\"")`, `("t"."a" = $1 OR "t"."b" = $2)`, []any{"p,q", `say "hi"`}},
		{"or", `(a.cs.{x,y},b.cs.{"k":"v","n":1},c.eq.3)`, `("t"."a" @> $1 OR "t"."b" @> $2 OR "t"."c" = $3)`, []any{"{x,y}", `{"k":"v","n":1}`, "3"}},
		{"or", `(a.fts(english).fox,b.wfts.dog)`, `("t"."a" @@ to_tsquery($1, $2) OR "t"."b" @@ websearch_to_tsquery($3))`, []any{"english", "fox", "dog"}},
		{"or", `( a.eq.1 , b.eq.2 )`, `("t"."a" = $1 OR "t"."b" = $2)`, []any{"1 ", "2 "}},
	} {
		read := &pgrRead{}
		got, err := read.logicTree(c.op, c.value)
		require.NoError(t, err, c.op+c.value)
		require.Equal(t, c.sql, got, c.op+c.value)
		if c.args != nil {
			require.Equal(t, c.args, read.args, c.op+c.value)
		}
	}
}

// Each refusal is PGRST100 with the message and details PostgREST 14 gives
// (the text the parser failed at, and what it expected, per Parsec).
func TestPgrRefusalsAreThoseOfPostgREST14(t *testing.T) {
	for _, c := range []struct{ expr, message, details string }{
		{"val", `"failed to parse filter (val)" (line 1, column 1)`, `unexpected "v" expecting "not" or operator (eq, gt, ...)`},
		{"nope.x", `"failed to parse filter (nope.x)" (line 1, column 1)`, `unexpected "p" expecting "not" or operator (eq, gt, ...)`},
		{"not.nope.x", `"failed to parse filter (not.nope.x)" (line 1, column 5)`, `unexpected "n" expecting operator (eq, gt, ...)`},
		{"fts().x", `"failed to parse filter (fts().x)" (line 1, column 5)`, `unexpected ")" expecting operator (eq, gt, ...)`},
		{"eq().x", `"failed to parse filter (eq().x)" (line 1, column 4)`, `unexpected ")" expecting operator (eq, gt, ...)`},
		{"is().x", `"failed to parse filter (is().x)" (line 1, column 3)`, `unexpected "(" expecting operator (eq, gt, ...)`},
		{"in().x", `"failed to parse filter (in().x)" (line 1, column 3)`, `unexpected "(" expecting operator (eq, gt, ...)`},
		{"neq(any).x", `"failed to parse filter (neq(any).x)" (line 1, column 4)`, `unexpected "(" expecting operator (eq, gt, ...)`},
		{"cs(all).x", `"failed to parse filter (cs(all).x)" (line 1, column 3)`, `unexpected "(" expecting operator (eq, gt, ...)`},
		{"fts(english.x", `"failed to parse filter (fts(english.x)" (line 1, column 12)`, `unexpected "." expecting operator (eq, gt, ...)`},
		{"cs", `"failed to parse filter (cs)" (line 1, column 3)`, `unexpected end of input expecting operator (eq, gt, ...)`},
		{"is.maybe", `"failed to parse filter (is.maybe)" (line 1, column 4)`, `unexpected "m" expecting isVal: (null, not_null, true, false, unknown)`},
		{"in.1", `"failed to parse filter (in.1)" (line 1, column 4)`, `unexpected "1" expecting "("`},
	} {
		_, err := (&pgrRead{}).predicate("c", c.expr)
		var refused *pgrError
		require.ErrorAs(t, err, &refused, c.expr)
		require.Equal(t, 400, refused.status, c.expr)
		require.Equal(t, "PGRST100", refused.code, c.expr)
		require.Equal(t, c.message, refused.message, c.expr)
		require.Equal(t, c.details, refused.details, c.expr)
	}

	// A logic tree is placed in the key's operator and the value together.
	for _, c := range []struct{ op, value, message, details string }{
		{"or", `()`, `"failed to parse logic tree (())" (line 1, column 4)`,
			`unexpected ")" expecting field name (* or [a..z0..9_$]), negation operator (not) or logic operator (and, or)`},
		{"or", `(id.in.1,2,id.eq.3)`, `"failed to parse logic tree ((id.in.1,2,id.eq.3))" (line 1, column 10)`, `unexpected "1" expecting "("`},
		{"or", `(id.eq.1,not.xor(id.eq.2,id.eq.3))`, `"failed to parse logic tree ((id.eq.1,not.xor(id.eq.2,id.eq.3)))" (line 1, column 16)`,
			`unexpected "x" expecting logic operator (and, or)`},
		{"or", `(a.foo.1)`, `"failed to parse logic tree ((a.foo.1))" (line 1, column 6)`, `unexpected "f" expecting "not" or operator (eq, gt, ...)`},
		{"or", `(a.eq.1`, `"failed to parse logic tree ((a.eq.1)" (line 1, column 10)`, `unexpected end of input expecting "," or ")"`},
	} {
		_, err := (&pgrRead{}).logicTree(c.op, c.value)
		var refused *pgrError
		require.ErrorAs(t, err, &refused, c.op+c.value)
		require.Equal(t, "PGRST100", refused.code, c.op+c.value)
		require.Equal(t, c.message, refused.message, c.op+c.value)
		require.Equal(t, c.details, refused.details, c.op+c.value)
	}
}
