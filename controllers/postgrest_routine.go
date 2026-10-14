package controllers

// miniship (miniship-cloud#1304, public ADR 0077): the functions of the
// Project's Database that /rest/v1/rpc/{function} can call, found the way
// PostgREST v12.2.3 finds them, and the refusals it gives when it cannot.
//
//   - The catalog query is SchemaCache.hs's funcsSqlQuery, narrowed to one name
//     in `public` and read in the call's own transaction, once for each call
//     and cached nowhere, as the foreign keys of an embed are. Where
//     funcsSqlQuery zips proargnames with proargtypes, which lists only the
//     input arguments and so misaligns a function with an OUT argument before
//     an IN one, this reads proallargtypes and the modes, and numbers the input
//     arguments among themselves.
//   - Which overload a call means is Plan.hs's findProc: the argument names
//     decide, not their types.
//   - A function that is not found is PGRST202 and one that is ambiguous is
//     PGRST203, in Error.hs's words (NoRpc and AmbiguousRpc).

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
)

// pgrParam is one input argument of a function.
type pgrParam struct {
	// name is "" for an argument the function left unnamed.
	name string
	// typ is the type's name as the catalog writes it, with the types that
	// ignore a length converted as SchemaCache.hs converts them: it is what
	// json_to_record reads the argument's value as.
	typ      string
	required bool
	variadic bool
}

// pgrResult is what a function returns, as Routine.hs's RetType and PgType
// sort it.
type pgrResult int

const (
	pgrResultVoid pgrResult = iota
	pgrResultScalar
	pgrResultSetOfScalar
	pgrResultComposite
	pgrResultSetOfComposite
)

// pgrRoutine is one function, or one overload of it.
type pgrRoutine struct {
	name        string
	params      []pgrParam
	result      pgrResult
	volatility  byte
	hasVariadic bool
	// unnamed is true when the function names none of its arguments.
	unnamed bool
}

// single is whether the function returns one value rather than a set
// (funcReturnsSingle).
func (r *pgrRoutine) single() bool {
	return r.result != pgrResultSetOfScalar && r.result != pgrResultSetOfComposite
}

// scalar is whether the function's value is one column, pgrst_scalar
// (funcReturnsScalar and funcReturnsSetOfScalar).
func (r *pgrRoutine) scalar() bool {
	return r.result == pgrResultScalar || r.result == pgrResultSetOfScalar || r.result == pgrResultVoid
}

// callable is SchemaCache.hs's `callable`: a function with an argument it did
// not name is called by this address only if it has exactly one, of a type a
// body can be passed as.
func (r *pgrRoutine) callable() bool {
	if !r.unnamed || len(r.params) == 0 {
		return true
	}
	if len(r.params) != 1 {
		return false
	}
	switch r.params[0].typ {
	case "bytea", "json", "jsonb", "text", "xml":
		return true
	}
	return false
}

// pgrRoutinesSQL is funcsSqlQuery, narrowed. Everything it names is a catalog
// column; the function's name is the one parameter.
const pgrRoutinesSQL = `
	SELECT p.proname::text,
	       COALESCE((
	         SELECT json_agg(json_build_object(
	                  'name', COALESCE(s.name, ''),
	                  'type', CASE s.type
	                            WHEN 'bit'::regtype THEN 'bit varying'
	                            WHEN 'bit[]'::regtype THEN 'bit varying[]'
	                            WHEN 'character'::regtype THEN 'character varying'
	                            WHEN 'character[]'::regtype THEN 'character varying[]'
	                            ELSE s.type::regtype::text
	                          END,
	                  'required', s.n <= (p.pronargs - p.pronargdefaults),
	                  'variadic', COALESCE(s.mode = 'v', false)) ORDER BY s.n)
	         FROM (
	           SELECT a.name, a.type, a.mode, row_number() OVER (ORDER BY a.idx) AS n
	           FROM unnest(p.proargnames, COALESCE(p.proallargtypes, p.proargtypes::oid[]), p.proargmodes)
	                WITH ORDINALITY AS a(name, type, mode, idx)
	           WHERE a.type IS NOT NULL AND COALESCE(a.mode, 'i') IN ('i', 'b', 'v')
	         ) s), '[]'::json)::text,
	       p.proargnames IS NULL,
	       p.proretset,
	       (t.typtype = 'c' OR COALESCE(p.proargmodes::text[] && '{t,b,o}', false)),
	       t.oid = 'void'::regtype,
	       p.provolatile::text,
	       p.provariadic > 0
	FROM pg_catalog.pg_proc p
	JOIN LATERAL (
	  WITH RECURSIVE r(oid, typbasetype) AS (
	    SELECT ty.oid, ty.typbasetype FROM pg_catalog.pg_type ty WHERE ty.oid = p.prorettype
	    UNION ALL
	    SELECT ty.oid, ty.typbasetype FROM r JOIN pg_catalog.pg_type ty ON ty.oid = r.typbasetype
	  )
	  SELECT r.oid FROM r WHERE r.typbasetype = 0
	) b ON true
	JOIN pg_catalog.pg_type t ON t.oid = b.oid
	WHERE p.pronamespace = 'public'::regnamespace AND p.prokind = 'f'
	  AND t.oid <> 'trigger'::regtype AND p.proname = $1`

// pgrFunctionNamesSQL is every function name `public` has, for the hint a
// misspelt name is given.
const pgrFunctionNamesSQL = `
	SELECT DISTINCT p.proname::text
	FROM pg_catalog.pg_proc p
	WHERE p.pronamespace = 'public'::regnamespace AND p.prokind = 'f' AND p.prorettype <> 'trigger'::regtype
	ORDER BY 1`

// pgrLoadRoutines is every overload of the function called name in `public`,
// the one with fewest arguments first, as Routine.hs orders them.
func pgrLoadRoutines(ctx context.Context, tx *sql.Tx, name string) ([]pgrRoutine, error) {
	rows, err := tx.QueryContext(ctx, pgrRoutinesSQL, name)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var routines []pgrRoutine
	for rows.Next() {
		var (
			routine         pgrRoutine
			args, volatile  string
			isSet, isRecord bool
			isVoid          bool
		)
		if err := rows.Scan(&routine.name, &args, &routine.unnamed, &isSet, &isRecord, &isVoid, &volatile, &routine.hasVariadic); err != nil {
			return nil, err
		}
		var decoded []struct {
			Name     string `json:"name"`
			Type     string `json:"type"`
			Required bool   `json:"required"`
			Variadic bool   `json:"variadic"`
		}
		if err := json.Unmarshal([]byte(args), &decoded); err != nil {
			return nil, err
		}
		for _, d := range decoded {
			routine.params = append(routine.params, pgrParam{name: d.Name, typ: d.Type, required: d.Required, variadic: d.Variadic})
		}
		if volatile != "" {
			routine.volatility = volatile[0]
		}
		switch {
		case isVoid:
			routine.result = pgrResultVoid
		case isRecord && isSet:
			routine.result = pgrResultSetOfComposite
		case isRecord:
			routine.result = pgrResultComposite
		case isSet:
			routine.result = pgrResultSetOfScalar
		default:
			routine.result = pgrResultScalar
		}
		routines = append(routines, routine)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	slices.SortStableFunc(routines, pgrCompareRoutines)
	return routines, nil
}

// pgrCompareRoutines is Routine.hs's Ord: the fewest arguments first, and then
// the arguments themselves, by name and by type, so that the order a message
// lists overloads in does not depend on the Database's.
func pgrCompareRoutines(a, b pgrRoutine) int {
	if len(a.params) != len(b.params) {
		return len(a.params) - len(b.params)
	}
	for i := range a.params {
		if c := strings.Compare(a.params[i].name, b.params[i].name); c != 0 {
			return c
		}
		if c := strings.Compare(a.params[i].typ, b.params[i].typ); c != 0 {
			return c
		}
	}
	return 0
}

// pgrLoadFunctionNames is the names of every function in `public`.
func pgrLoadFunctionNames(ctx context.Context, tx *sql.Tx) ([]string, error) {
	rows, err := tx.QueryContext(ctx, pgrFunctionNamesSQL)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		names = append(names, name)
	}
	return names, rows.Err()
}

// pgrMatches is Plan.hs's matchesParams: whether the argument names a call
// sends are the names this overload takes. A function has optional arguments,
// which carry a default, and required ones; the names must cover every
// required one and may add any optional one and nothing else.
func (r *pgrRoutine) matches(keys map[string]bool, singleObject bool) bool {
	if singleObject {
		return len(r.params) == 1 && (r.params[0].typ == "json" || r.params[0].typ == "jsonb")
	}
	if len(r.params) == 0 {
		return len(keys) == 0
	}
	required, optional := map[string]bool{}, map[string]bool{}
	for _, p := range r.params {
		if p.required {
			required[p.name] = true
		} else {
			optional[p.name] = true
		}
	}
	switch {
	case len(optional) == 0:
		return pgrSameSet(keys, required)
	case len(required) == 0:
		for key := range keys {
			if !optional[key] {
				return false
			}
		}
		return true
	}
	rest := map[string]bool{}
	for key := range keys {
		if !optional[key] {
			rest[key] = true
		}
	}
	return pgrSameSet(rest, required)
}

func pgrSameSet(a, b map[string]bool) bool {
	if len(a) != len(b) {
		return false
	}
	for key := range a {
		if !b[key] {
			return false
		}
	}
	return true
}

// takesBody is Plan.hs's hasSingleUnnamedParam for a JSON body: a function of
// one json or jsonb argument can be handed the whole body when no overload
// takes its keys by name.
func (r *pgrRoutine) takesBody() bool {
	return len(r.params) == 1 && (r.params[0].typ == "json" || r.params[0].typ == "jsonb")
}

// pgrFindRoutine is findProc. keys are the argument names the call sent.
func pgrFindRoutine(name string, routines []pgrRoutine, keys map[string]bool, singleObject, isPost bool) (*pgrRoutine, []pgrRoutine, error) {
	var matched, fallback []pgrRoutine
	for _, r := range routines {
		if !r.callable() {
			continue
		}
		switch {
		case r.matches(keys, singleObject):
			matched = append(matched, r)
		case isPost && r.takesBody():
			fallback = append(fallback, r)
		}
	}
	switch {
	case len(matched) == 0 && len(fallback) == 0:
		return nil, nil, errNoRoutine
	case len(matched) == 0 && len(fallback) == 1:
		return &fallback[0], nil, nil
	case len(matched) == 0:
		return nil, fallback, errAmbiguousRoutine
	case len(matched) == 1:
		return &matched[0], nil, nil
	}
	return nil, matched, errAmbiguousRoutine
}

type pgrRoutineError string

func (e pgrRoutineError) Error() string { return string(e) }

const (
	errNoRoutine        = pgrRoutineError("no function")
	errAmbiguousRoutine = pgrRoutineError("ambiguous function")
)

// pgrNoRoutine is Error.hs's NoRpc, PGRST202: the message, the details and the
// hint, in its words. overloads are the functions of the name asked for;
// others every function name `public` has.
func pgrNoRoutine(name string, keys []string, singleObject, isPost bool, overloads []pgrRoutine, others []string) error {
	function := pgrSchema + "." + name
	slices.Sort(keys)
	listed := strings.Join(keys, ", ")
	parameters := func() string {
		if len(keys) == 0 {
			return " without parameters"
		}
		if len(keys) > 1 {
			return " with parameters " + listed
		}
		return " with parameter " + listed
	}
	message := "Could not find the function " + function
	switch {
	case singleObject:
	case len(keys) == 0:
		message += " without parameters"
	default:
		message += "(" + listed + ")"
	}
	message += " in the schema cache"

	details := "Searched for the function " + function
	switch {
	case singleObject:
		details += " with a single json/jsonb parameter"
	case isPost:
		details += parameters() + " or with a single unnamed json/jsonb parameter"
	default:
		details += parameters()
	}
	details += ", but no matches were found in the schema cache."

	failure := &pgrError{status: http.StatusNotFound, code: "PGRST202", message: message, details: details}
	if singleObject {
		return failure
	}
	if len(overloads) == 0 {
		if near, ok := pgrClosest(name, others); ok {
			failure.hint = "Perhaps you meant to call the function " + pgrSchema + "." + near
		}
		return failure
	}
	var shapes []string
	for _, overload := range overloads {
		names := make([]string, 0, len(overload.params))
		for _, p := range overload.params {
			names = append(names, p.name)
		}
		slices.Sort(names)
		shape := "(" + strings.Join(names, ", ") + ")"
		shapes = append(shapes, shape)
	}
	if near, ok := pgrClosest("("+listed+")", shapes); ok {
		failure.hint = "Perhaps you meant to call the function " + pgrSchema + "." + name + near
	}
	return failure
}

// pgrAmbiguousRoutine is Error.hs's AmbiguousRpc, PGRST203.
func pgrAmbiguousRoutine(candidates []pgrRoutine) error {
	described := make([]string, 0, len(candidates))
	for _, c := range candidates {
		params := make([]string, 0, len(c.params))
		for _, p := range c.params {
			params = append(params, p.name+" => "+p.typ)
		}
		described = append(described, fmt.Sprintf("%s.%s(%s)", pgrSchema, c.name, strings.Join(params, ", ")))
	}
	return &pgrError{
		status:  http.StatusMultipleChoices,
		code:    "PGRST203",
		message: "Could not choose the best candidate function between: " + strings.Join(described, ", "),
		hint:    "Try renaming the parameters or the function itself in the database so function overloading can be resolved",
	}
}
