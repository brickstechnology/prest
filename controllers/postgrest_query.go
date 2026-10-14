package controllers

// miniship (miniship-cloud#1302, public ADR 0077): PostgREST's query language
// for a read, as the Supabase SDK's database client writes it. Reads only:
// the columns named, the filters eq, neq, gt, gte, lt, lte, like, ilike, is,
// in, each of them under not., an or=( … ) group, order, limit and offset.
// Writes, function calls and reads that embed another table are their own
// tickets (#1303, #1304, #1305) and are refused here, by name.
//
// Nothing the caller sent becomes SQL text but a name that passed pgrIdent and
// was quoted: every value is a parameter. The parser is written against the
// query string the SDK's postgrest-js builds (its PostgrestFilterBuilder and
// PostgrestTransformBuilder), not against the whole of PostgREST's grammar.

import (
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/lib/pq"
)

// pgrSchema is the one schema the address serves, as the table read serves.
const pgrSchema = servedSchema

// pgrIdent is a plain Postgres name: what a column or a table may be called
// here. A quoted name, a JSON path, a cast and an alias are not answered yet.
var pgrIdent = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,62}$`)

// pgrError is a refusal in PostgREST's shape: a status and the four fields the
// SDK reads, message, code, details and hint.
type pgrError struct {
	status  int
	code    string
	message string
	details string
	hint    string
}

func (e *pgrError) Error() string { return e.message }

// pgrRefuse is a refusal of the query itself, PGRST100 as PostgREST names a
// query string it cannot parse.
func pgrRefuse(message string) error {
	return &pgrError{status: 400, code: "PGRST100", message: message}
}

func pgrQuote(name string) string { return pq.QuoteIdentifier(name) }

func pgrTable(name string) string { return pgrQuote(pgrSchema) + "." + pgrQuote(name) }

// pgrCheckName refuses a name that is not plain, and quotes nothing of it back.
func pgrCheckName(name, what string) error {
	if !pgrIdent.MatchString(name) {
		return pgrRefuse(fmt.Sprintf("a %s is a plain name of letters, digits and _", what))
	}
	return nil
}

// pgrSplit splits s on commas outside parentheses and outside double quotes;
// a double-quoted run keeps its commas and loses its quotes.
func pgrSplit(s string) []string {
	var out []string
	var current strings.Builder
	depth := 0
	quoted := false
	for _, ch := range s {
		switch {
		case ch == '"':
			quoted = !quoted
		case ch == '(' && !quoted:
			depth++
			current.WriteRune(ch)
		case ch == ')' && !quoted:
			depth--
			current.WriteRune(ch)
		case ch == ',' && !quoted && depth == 0:
			out = append(out, current.String())
			current.Reset()
		default:
			current.WriteRune(ch)
		}
	}
	return append(out, current.String())
}

// pgrRead is one read, parsed: what to select, the predicates ANDed, the
// order, and the page.
type pgrRead struct {
	columns []string
	where   []string
	args    []any
	order   string
	limit   int
	offset  int
}

func (q *pgrRead) arg(v any) string {
	q.args = append(q.args, v)
	return fmt.Sprintf("$%d", len(q.args))
}

// The query string's own keys, which are not filters.
var pgrReserved = map[string]bool{"select": true, "order": true, "limit": true, "offset": true}

// pgrParseRead parses a read's query string. maxRows is the page ceiling: a
// read that names no limit is given it, and one that names a larger limit is
// refused, because the SDK reads a refusal and would read a shorter page as
// the whole answer.
func pgrParseRead(query url.Values, maxRows int) (*pgrRead, error) {
	read := &pgrRead{limit: maxRows}
	columns, err := pgrParseSelect(query.Get("select"))
	if err != nil {
		return nil, err
	}
	read.columns = columns

	keys := make([]string, 0, len(query))
	for key := range query {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if pgrReserved[key] {
			continue
		}
		for _, value := range query[key] {
			var predicate string
			switch key {
			case "or":
				predicate, err = read.group(value, " OR ")
			case "and":
				predicate, err = read.group(value, " AND ")
			case "not.or", "not.and":
				join := " OR "
				if key == "not.and" {
					join = " AND "
				}
				predicate, err = read.group(value, join)
				predicate = "NOT " + predicate
			default:
				predicate, err = read.predicate(key, value)
			}
			if err != nil {
				return nil, err
			}
			read.where = append(read.where, predicate)
		}
	}

	if read.order, err = pgrParseOrder(query.Get("order")); err != nil {
		return nil, err
	}
	if raw := query.Get("limit"); raw != "" {
		n, convErr := strconv.Atoi(raw)
		if convErr != nil || n < 0 {
			return nil, pgrRefuse("limit is a whole number")
		}
		if n > maxRows {
			return nil, &pgrError{
				status:  400,
				code:    "MS100",
				message: "the read asks for more rows than rest answers at once",
				details: fmt.Sprintf("rest answers at most %d rows a read", maxRows),
				hint:    fmt.Sprintf("ask for %d or fewer, with limit or range, and page through the rest", maxRows),
			}
		}
		read.limit = n
	}
	if raw := query.Get("offset"); raw != "" {
		n, convErr := strconv.Atoi(raw)
		if convErr != nil || n < 0 {
			return nil, pgrRefuse("offset is a whole number")
		}
		read.offset = n
	}
	return read, nil
}

// pgrParseSelect reads `select=a,b` or `*`. An embedded table, an alias, a
// cast and a JSON path are refused by name rather than misread.
func pgrParseSelect(raw string) ([]string, error) {
	if strings.TrimSpace(raw) == "" {
		return []string{"*"}, nil
	}
	var columns []string
	for _, item := range pgrSplit(raw) {
		item = strings.TrimSpace(item)
		switch {
		case item == "*":
			columns = append(columns, "*")
		case strings.Contains(item, "("):
			return nil, pgrRefuse("a read that embeds another table is not answered here yet")
		case strings.ContainsAny(item, ":->"):
			return nil, pgrRefuse("a column alias, cast or JSON path is not answered here yet")
		default:
			if err := pgrCheckName(item, "column"); err != nil {
				return nil, err
			}
			columns = append(columns, item)
		}
	}
	return columns, nil
}

// predicate turns one filter, `op.value` or `not.op.value` on column, into SQL.
func (q *pgrRead) predicate(column, expr string) (string, error) {
	if err := pgrCheckName(column, "column"); err != nil {
		return "", err
	}
	negate := false
	if rest, found := strings.CutPrefix(expr, "not."); found {
		negate, expr = true, rest
	}
	op, value, found := strings.Cut(expr, ".")
	if !found {
		return "", pgrRefuse("a filter is written column=operator.value")
	}
	c := `"t".` + pgrQuote(column)
	var text string
	switch op {
	case "eq":
		text = c + " = " + q.arg(value)
	case "neq":
		text = c + " <> " + q.arg(value)
	case "gt":
		text = c + " > " + q.arg(value)
	case "gte":
		text = c + " >= " + q.arg(value)
	case "lt":
		text = c + " < " + q.arg(value)
	case "lte":
		text = c + " <= " + q.arg(value)
	case "like":
		text = c + " LIKE " + q.arg(strings.ReplaceAll(value, "*", "%"))
	case "ilike":
		text = c + " ILIKE " + q.arg(strings.ReplaceAll(value, "*", "%"))
	case "is":
		switch strings.ToLower(value) {
		case "null":
			text = c + " IS NULL"
		case "true":
			text = c + " IS TRUE"
		case "false":
			text = c + " IS FALSE"
		case "unknown":
			text = c + " IS UNKNOWN"
		default:
			return "", pgrRefuse("is takes null, true, false or unknown")
		}
	case "in":
		items, err := pgrList(value)
		if err != nil {
			return "", err
		}
		if len(items) == 0 {
			text = "FALSE"
			break
		}
		holes := make([]string, 0, len(items))
		for _, item := range items {
			holes = append(holes, q.arg(item))
		}
		text = c + " IN (" + strings.Join(holes, ", ") + ")"
	default:
		return "", pgrRefuse("this operator is not answered here yet: the read answers eq, neq, gt, gte, lt, lte, like, ilike, is and in")
	}
	if negate {
		text = "NOT (" + text + ")"
	}
	return text, nil
}

// pgrList reads `(a,b,"c,d")`.
func pgrList(value string) ([]string, error) {
	v := strings.TrimSpace(value)
	if !strings.HasPrefix(v, "(") || !strings.HasSuffix(v, ")") {
		return nil, pgrRefuse("an in list is written (a,b,c)")
	}
	inner := v[1 : len(v)-1]
	if inner == "" {
		return nil, nil
	}
	return pgrSplit(inner), nil
}

// group reads `(a.eq.1,b.gt.2)`, and a nested `and(…)`, `or(…)`, `not.and(…)`
// or `not.or(…)` inside it, into one parenthesised group.
func (q *pgrRead) group(value, join string) (string, error) {
	v := strings.TrimSpace(value)
	if !strings.HasPrefix(v, "(") || !strings.HasSuffix(v, ")") {
		return "", pgrRefuse("a group is written or=(a.eq.1,b.eq.2)")
	}
	var predicates []string
	for _, part := range pgrSplit(v[1 : len(v)-1]) {
		part = strings.TrimSpace(part)
		var predicate string
		var err error
		switch {
		case strings.HasPrefix(part, "or("):
			predicate, err = q.group(part[len("or"):], " OR ")
		case strings.HasPrefix(part, "and("):
			predicate, err = q.group(part[len("and"):], " AND ")
		case strings.HasPrefix(part, "not.or("):
			predicate, err = q.group(part[len("not.or"):], " OR ")
			predicate = "NOT " + predicate
		case strings.HasPrefix(part, "not.and("):
			predicate, err = q.group(part[len("not.and"):], " AND ")
			predicate = "NOT " + predicate
		default:
			column, expr, found := strings.Cut(part, ".")
			if !found {
				return "", pgrRefuse("an item of a group is written column.operator.value")
			}
			predicate, err = q.predicate(column, expr)
		}
		if err != nil {
			return "", err
		}
		predicates = append(predicates, predicate)
	}
	return "(" + strings.Join(predicates, join) + ")", nil
}

// pgrParseOrder reads `col.desc.nullslast,other`.
func pgrParseOrder(raw string) (string, error) {
	if strings.TrimSpace(raw) == "" {
		return "", nil
	}
	var parts []string
	for _, item := range pgrSplit(raw) {
		segments := strings.Split(strings.TrimSpace(item), ".")
		if err := pgrCheckName(segments[0], "column"); err != nil {
			return "", err
		}
		direction, nulls := "ASC", ""
		for _, segment := range segments[1:] {
			switch segment {
			case "asc":
				direction = "ASC"
			case "desc":
				direction = "DESC"
			case "nullsfirst":
				nulls = " NULLS FIRST"
			case "nullslast":
				nulls = " NULLS LAST"
			default:
				return "", pgrRefuse("order takes asc, desc, nullsfirst or nullslast")
			}
		}
		parts = append(parts, `"t".`+pgrQuote(segments[0])+" "+direction+nulls)
	}
	return " ORDER BY " + strings.Join(parts, ", "), nil
}

// whereSQL is the predicates, ANDed, as a WHERE clause.
func (q *pgrRead) whereSQL() string {
	if len(q.where) == 0 {
		return ""
	}
	return " WHERE " + strings.Join(q.where, " AND ")
}

// rowsSQL is the page of rows as one JSON array of objects, in the order the
// columns were named.
func (q *pgrRead) rowsSQL(table string) string {
	selected := make([]string, 0, len(q.columns))
	for _, column := range q.columns {
		if column == "*" {
			selected = append(selected, `"t".*`)
			continue
		}
		selected = append(selected, `"t".`+pgrQuote(column))
	}
	return fmt.Sprintf(`SELECT coalesce(json_agg(x), '[]'::json)::text FROM (SELECT %s FROM %s AS "t"%s%s LIMIT %d OFFSET %d) AS x`,
		strings.Join(selected, ", "), pgrTable(table), q.whereSQL(), q.order, q.limit, q.offset)
}

// countSQL is how many rows the filters match, before the page.
func (q *pgrRead) countSQL(table string) string {
	return fmt.Sprintf(`SELECT count(*) FROM %s AS "t"%s`, pgrTable(table), q.whereSQL())
}
