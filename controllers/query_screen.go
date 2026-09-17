package controllers

// miniship (#549): the second wall.
//
// rest is the one door the internet reaches directly. Since miniship-cloud#546
// a table read runs as the Database's anonymous role, inside a read-only
// transaction, over public alone — that is the floor, and it is the Database's
// own grants. This file is the wall above it, in rest itself, so that a mistake
// in those grants is not enough to reach anything.
//
// It is an allowlist by construction: the builders downstream never see the
// caller's query string, only the one rebuilt here out of parameters this file
// names and values this file's own grammar accepts. A parameter upstream grows
// later is therefore refused until somebody reviews it and adds it, rather than
// served the day it appears.
//
// docs/miniship/query-string.md is the review: every parameter, kept,
// restricted or removed, and why. Keep the two in step.

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

// QueryBounds are the limits a table read is held to. Zero means the default.
type QueryBounds struct {
	// MaxPageSize is the largest page rest will serve. A larger request is
	// capped to it, not refused, and a read that asks for no page at all is
	// given this one — so no read is unbounded.
	MaxPageSize int
	// MaxQueryLen is the largest query string rest will read, in bytes. It
	// bounds the work done before the page ceiling and the time limit can
	// bound anything: a query string is as long as the request line allows,
	// and every filter in it is a predicate built and a parameter bound.
	MaxQueryLen int
}

// The defaults, both taken from docs/research/QUERY-BOUNDS-AND-FAIRNESS.md and
// both named in docs/miniship/query-string.md: Loki's
// max_entries_limit_per_query, the one rows-returned limit in that survey that
// ships with a real non-zero default, and VictoriaLogs' -search.maxQueryLen,
// the one query-complexity bound in it expressed as a hard byte cap.
const (
	defaultMaxPageSize = 5000
	defaultMaxQueryLen = 16384
)

// withDefaults returns b with every unset bound at its default.
func (b QueryBounds) withDefaults() QueryBounds {
	if b.MaxPageSize <= 0 {
		b.MaxPageSize = defaultMaxPageSize
	}
	if b.MaxQueryLen <= 0 {
		b.MaxQueryLen = defaultMaxQueryLen
	}
	return b
}

// queryTooLong is the one refusal that is not ErrQueryNotAccepted: the query
// string was not read at all, so there is no parameter to name. It answers 414,
// which is what a request line too long to serve is.
const queryTooLong = "the query string is longer than rest reads"

// ErrQueryNotAccepted is every refusal this file makes: the request is not one
// rest serves, and nothing was read. Its message names the rule and quotes
// nothing of the request — not the value, and not the parameter's name either —
// so an error answer cannot be used to reflect a caller's bytes.
var ErrQueryNotAccepted = errors.New("query string not accepted")

// refuse builds a refusal that states the rule and quotes nothing.
func refuse(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrQueryNotAccepted, fmt.Sprintf(format, args...))
}

var (
	// identRe is one unqualified SQL identifier. A dot is what qualifies a
	// name with a schema or a table, which is how a read leaves public, so no
	// parameter here takes one.
	identRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,62}$`)
	// digitsRe is a page number or a page size, short enough that no value
	// here can overflow the int it is parsed into.
	digitsRe = regexp.MustCompile(`^[0-9]{1,9}$`)
	// operatorRe is upstream's own operator shape, character for character —
	// `adapters/postgres/postgres.go`'s removeOperatorRegex, whose last `.` is
	// **not** escaped and so matches any byte, not only a dot. Writing the
	// dot here instead would leave a hole rather than close one: upstream
	// reads `$ltreematch x` as the ltree match operator, and a screen looking
	// for `$ltreematch.` would see no operator at all and wave the value
	// through.
	//
	// Upstream finds this anywhere in a value and strips every occurrence. The
	// screen requires exactly one, at the front, so a value that merely
	// contains another is refused rather than silently rewritten into a filter
	// the caller did not ask for.
	operatorRe = regexp.MustCompile(`\$[a-z]+.`)
	// operatorName undoes what upstream does to a match before it looks the
	// operator up: the dot here, the `$` and the spaces in GetQueryOperator.
	operatorName = strings.NewReplacer(".", "", "$", "", " ", "")
)

// aggregates are the six aggregate functions upstream's `FUNC:field[:alias]`
// spelling builds. Nothing else is a function call rest accepts.
var aggregates = map[string]struct{}{
	"sum": {}, "avg": {}, "max": {}, "min": {}, "stddev": {}, "variance": {},
}

// filterOperators are the comparisons a column filter may name. It is this
// file's own list, deliberately not adapters/postgres's: an operator upstream
// adds is refused here until it is reviewed. TestScreenOperators_areASubsetOfUpstreams
// holds it to being a subset of upstream's, so a kept operator always resolves.
//
// Upstream's four ltree operators (`$ltreelanc`, `$ltreerdesc`, `$ltreematch`,
// `$ltreematchtxt`) are not here: on a text column `~` is a POSIX regular
// expression, which is caller-supplied backtracking, and no miniship Project
// schema has an ltree.
var filterOperators = map[string]struct{}{
	"eq": {}, "ne": {}, "gt": {}, "gte": {}, "lt": {}, "lte": {},
	"in": {}, "nin": {}, "any": {}, "some": {}, "all": {},
	"null": {}, "notnull": {}, "true": {}, "nottrue": {},
	"false": {}, "notfalse": {},
	"like": {}, "ilike": {}, "nlike": {}, "nilike": {},
}

// screenTableRead reviews the query string of a table read and returns the one
// the builders may see: a fresh url.Values holding only what this file put
// there. An error means the request is refused and nothing was read.
func screenTableRead(in url.Values, bounds QueryBounds) (url.Values, error) {
	bounds = bounds.withDefaults()
	out := url.Values{}

	for key, values := range in {
		if !strings.HasPrefix(key, "_") {
			if err := screenFilter(out, key, values); err != nil {
				return nil, err
			}
			continue
		}
		if err := screenReserved(out, key, values); err != nil {
			return nil, err
		}
	}

	bound(out, bounds)
	return out, nil
}

// screenReserved screens one `_`-prefixed parameter. The switch is the whole
// list of parameters rest serves: default refuses everything else, which is
// what makes this an allowlist rather than a filter.
func screenReserved(out url.Values, key string, values []string) error {
	switch key {
	case "_select":
		// One comma-separated value, not one value per field: upstream reads
		// _select at two sinks, and CountByRequest takes only the first value
		// of the parameter, so a second value would be a column the count
		// silently dropped.
		fields := []string{}
		for _, v := range values {
			for _, field := range strings.Split(v, ",") {
				field = strings.TrimSpace(field)
				if field == "" {
					continue
				}
				if !isSelectField(field) {
					return refuse("_select takes column names, * or one of SUM AVG MAX MIN STDDEV VARIANCE as FUNC:column[:alias]")
				}
				fields = append(fields, field)
			}
		}
		if len(fields) > 0 {
			out.Set("_select", strings.Join(fields, ","))
		}
	case "_count":
		v, err := one(key, values)
		if err != nil {
			return err
		}
		// COUNT takes one argument, so _count names one column or *.
		if v != "*" && !identRe.MatchString(v) {
			return refuse("_count takes one column name, or *")
		}
		out.Set("_count", v)
	case "_count_first":
		v, err := one(key, values)
		if err != nil {
			return err
		}
		if v != "true" {
			return refuse("_count_first takes true")
		}
		out.Set("_count_first", v)
	case "_distinct":
		v, err := one(key, values)
		if err != nil {
			return err
		}
		if v != "true" && v != "false" {
			return refuse("_distinct takes true or false")
		}
		out.Set("_distinct", v)
	case "_order":
		v, err := one(key, values)
		if err != nil {
			return err
		}
		list, ok := nameList(v, func(field string) bool {
			return identRe.MatchString(strings.TrimPrefix(field, "-"))
		})
		if !ok {
			return refuse("_order takes column names, each optionally prefixed with - for descending")
		}
		out.Set("_order", list)
	case "_groupby":
		v, err := one(key, values)
		if err != nil {
			return err
		}
		list, ok := nameList(v, identRe.MatchString)
		if !ok {
			return refuse("_groupby takes column names")
		}
		out.Set("_groupby", list)
	case "_page", "_page_size":
		v, err := one(key, values)
		if err != nil {
			return err
		}
		if !digitsRe.MatchString(v) {
			return refuse("%s takes a whole number", key)
		}
		if n, _ := strconv.Atoi(v); n < 1 {
			return refuse("%s starts at 1", key)
		}
		out.Set(key, v)
	default:
		// Nothing of the caller's is quoted back, not even the name: the
		// answer says what rest does serve, which is more use to a developer
		// than an echo and cannot be used to reflect bytes.
		return refuse("that is not a parameter rest serves; they are %s, and a column name",
			strings.Join(servedParameters, " "))
	}
	return nil
}

// servedParameters is the list an unknown parameter is answered with. It is
// the same list as the switch above, and TestScreen_theAnswerNamesWhatIsServed
// holds the two together.
var servedParameters = []string{
	"_select", "_count", "_count_first", "_distinct",
	"_order", "_groupby", "_page", "_page_size",
}

// nameList validates every comma-separated item of v with ok and returns the
// list re-joined from its trimmed items, so what the builders read is the
// screen's own spelling rather than the caller's.
func nameList(v string, ok func(string) bool) (string, bool) {
	fields := strings.Split(v, ",")
	for i, field := range fields {
		field = strings.TrimSpace(field)
		if !ok(field) {
			return "", false
		}
		fields[i] = field
	}
	return strings.Join(fields, ","), true
}

// screenFilter screens one column filter, `column=$op.value` or
// `column->>key:jsonb=$op.value`. The value itself is bound as a parameter by
// the builder, so what is screened is its shape: the column, the optional JSON
// key, and the operator.
func screenFilter(out url.Values, key string, values []string) error {
	name := key
	if suffix := strings.Index(key, ":"); suffix >= 0 {
		if key[suffix+1:] != "jsonb" {
			return refuse("the only typed filter rest serves is column->>key:jsonb")
		}
		left, jsonKey, found := strings.Cut(key[:suffix], "->>")
		if !found || !identRe.MatchString(jsonKey) {
			return refuse("a jsonb filter is column->>key:jsonb, both plain names")
		}
		name = left
	}
	if !identRe.MatchString(name) {
		return refuse("a filter names one column of the table")
	}
	for _, v := range values {
		found := operatorRe.FindAllString(v, 2)
		if len(found) > 0 {
			if len(found) > 1 || !strings.HasPrefix(v, found[0]) {
				return refuse("a filter takes one operator, written at the front of its value as $op.")
			}
			if _, ok := filterOperators[operatorName.Replace(found[0])]; !ok {
				return refuse("that is not an operator rest serves")
			}
		}
		out.Add(key, v)
	}
	return nil
}

// bound holds the read to its page. A page larger than the ceiling is capped
// rather than refused, and a read that asked for no page at all is given the
// ceiling, so every read carries a LIMIT.
func bound(out url.Values, bounds QueryBounds) {
	if out.Get("_page") == "" {
		out.Set("_page", "1")
		if out.Get("_page_size") == "" {
			out.Set("_page_size", strconv.Itoa(bounds.MaxPageSize))
		}
	}
	// The value is digits, screened above, so it parses.
	if n, err := strconv.Atoi(out.Get("_page_size")); err == nil && n > bounds.MaxPageSize {
		out.Set("_page_size", strconv.Itoa(bounds.MaxPageSize))
	}
}

// isSelectField reports whether field is one _select item rest serves.
func isSelectField(field string) bool {
	if field == "*" || identRe.MatchString(field) {
		return true
	}
	parts := strings.Split(field, ":")
	if len(parts) != 2 && len(parts) != 3 {
		return false
	}
	if _, ok := aggregates[strings.ToLower(parts[0])]; !ok {
		return false
	}
	if parts[1] != "*" && !identRe.MatchString(parts[1]) {
		return false
	}
	return len(parts) == 2 || identRe.MatchString(parts[2])
}

// one returns the single value of a parameter given once. A parameter given
// twice is refused rather than resolved to whichever the builder happens to
// read, so what rest ran is what the caller wrote.
func one(key string, values []string) (string, error) {
	if len(values) != 1 {
		return "", refuse("%s is given once", key)
	}
	return values[0], nil
}
