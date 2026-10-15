package controllers

// miniship (miniship-cloud#1302, public ADR 0077): PostgREST's query language
// for a read, as the Supabase SDK's database client writes it. Reads only:
// the columns named, the filters eq, neq, gt, gte, lt, lte, like, ilike, is,
// in, the array, range and jsonb operators cs, cd, ov, sl, sr, nxl, nxr and adj
// and the full-text operators fts, plfts, phfts and wfts (#1427), each of them
// under not., an or=( … ) group, order, limit and offset.
// Writes and function calls are their own tickets and have their own files
// (#1303 postgrest_write.go, #1304 postgrest_call.go), which parse their
// filters here. A read that embeds another table, an alias, a cast and a json
// path are answered (#1305): postgrest_select.go and postgrest_embed.go.
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
// here. A quoted name is not answered yet; a JSON path, a cast and an alias
// are read by postgrest_select.go around a name that passed this.
var pgrIdent = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,62}$`)

// pgrError is a refusal in PostgREST's shape: a status and the four fields the
// SDK reads, message, code, details and hint.
type pgrError struct {
	status  int
	code    string
	message string
	details string
	hint    string
	// detailsList is details when PostgREST answers a list, as PGRST201 does.
	detailsList any
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

// pgrSplitGroup splits the inside of an or / and group on its commas, as
// QueryParams.hs's `pLogicTree` does: a value is a double-quoted run, or a
// `{…}` run (a Postgres array or a flat json object, `pPgArray`), or the text up
// to the next comma or closing parenthesis. A `{…}` run is kept as written, with
// its quotes. A quoted run loses its quotes at the group's own level and keeps
// them inside a nested group, which splits it again.
func pgrSplitGroup(s string) []string {
	var out []string
	var current strings.Builder
	depth := 0
	quoted := false
	runes := []rune(s)
	for i := 0; i < len(runes); i++ {
		ch := runes[i]
		switch {
		case ch == '{' && !quoted && i > 0 && runes[i-1] == '.':
			end := i
			for end < len(runes) && runes[end] != '}' {
				end++
			}
			if end == len(runes) {
				end--
			}
			current.WriteString(string(runes[i : end+1]))
			i = end
		case ch == '"':
			quoted = !quoted
			if depth > 0 {
				current.WriteRune(ch)
			}
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
	items   []pgrItem
	where   []string
	args    []any
	order   string
	limit   int
	offset  int
	maxRows int

	// whereArgs is how many of args the filters use. A read that embeds adds
	// arguments to its select list, and a count is asked with the first
	// whereArgs alone, because Postgres refuses a parameter nothing uses.
	whereArgs int
	// root and alias are set on the reader of an embedded table's filters: its
	// columns are qualified by alias, and its arguments are the root read's.
	root  *pgrRead
	alias string
}

// tableAlias is what the columns of this read's filters are qualified by.
func (q *pgrRead) tableAlias() string {
	if q.alias != "" {
		return q.alias
	}
	return "t"
}

// countArgs are the arguments the count query uses.
func (q *pgrRead) countArgs() []any { return q.args[:q.whereArgs] }

func (q *pgrRead) arg(v any) string {
	if q.root != nil {
		return q.root.arg(v)
	}
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
	read := &pgrRead{limit: maxRows, maxRows: maxRows}
	raw := strings.TrimSpace(query.Get("select"))
	if raw == "" {
		raw = "*"
	}
	items, err := pgrParseSelectList(raw, 0)
	if err != nil {
		return nil, err
	}
	read.items = items

	keys := make([]string, 0, len(query))
	for key := range query {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if embed, local := pgrFindEmbed(read.items, key); embed != nil {
			for _, value := range query[key] {
				if err := embed.address(local, value, maxRows); err != nil {
					return nil, err
				}
			}
			continue
		}
		if pgrReserved[key] {
			continue
		}
		for _, value := range query[key] {
			if err := read.filter(key, value); err != nil {
				return nil, err
			}
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
			return nil, pgrTooManyRows(maxRows)
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
	read.whereArgs = len(read.args)
	return read, nil
}

// pgrTooManyRows is the refusal of a limit above the page ceiling.
func pgrTooManyRows(maxRows int) error {
	return &pgrError{
		status:  400,
		code:    "MS100",
		message: "the read asks for more rows than rest answers at once",
		details: fmt.Sprintf("rest answers at most %d rows a read", maxRows),
		hint:    fmt.Sprintf("ask for %d or fewer, with limit or range, and page through the rest", maxRows),
	}
}

// filter adds one filter of the query string, a column or an or / and group,
// to the read's predicates.
func (q *pgrRead) filter(key, value string) error {
	var predicate string
	var err error
	switch key {
	case "or":
		predicate, err = q.group(value, " OR ")
	case "and":
		predicate, err = q.group(value, " AND ")
	case "not.or", "not.and":
		join := " OR "
		if key == "not.and" {
			join = " AND "
		}
		predicate, err = q.group(value, join)
		predicate = "NOT " + predicate
	default:
		predicate, err = q.predicate(key, value)
	}
	if err != nil {
		return err
	}
	q.where = append(q.where, predicate)
	return nil
}

// predicate turns one filter, `op.value` or `not.op.value` on column, into SQL.
func (q *pgrRead) predicate(column, expr string) (string, error) {
	parsed, err := pgrParseColumn(column)
	if err != nil {
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
	c := parsed.sql(q.tableAlias())
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
		if symbol, ok := pgrSimpleOperators[op]; ok {
			// PostgREST hands the value to the Database as an unknown literal
			// (SqlFragment.hs, pgFmtFilter, `Op op val`), so `{a,b}`, a json
			// value and a range literal are read by the column's own type; so
			// is a parameter. A value that does not parse is the Database's
			// 22P02, and an operator the column's type lacks is its 42883.
			text = c + " " + symbol + " " + q.arg(value)
			break
		}
		fts := pgrFtsOperator.FindStringSubmatch(op)
		if fts == nil {
			return "", pgrRefuse("this operator is not answered: the read answers eq, neq, gt, gte, lt, lte, like, ilike, is, in, cs, cd, ov, sl, sr, nxl, nxr, adj, fts, plfts, phfts and wfts")
		}
		// `@@ to_tsquery(config, value)`, with the config left out when the
		// filter names none (SqlFragment.hs, `Fts op lang val`; QueryParams.hs,
		// `pFts`, where the config is an identifier in parentheses).
		call := pgrFtsFunctions[fts[1]] + "("
		if config := strings.TrimSpace(fts[2]); config != "" {
			call += q.arg(config) + ", "
		} else if fts[2] != "" {
			return "", pgrRefuse("a text search configuration is a plain name")
		}
		text = c + " @@ " + call + q.arg(value) + ")"
	}
	if negate {
		text = "NOT (" + text + ")"
	}
	return text, nil
}

// pgrSimpleOperators are the operators that take one value as it is written, and
// the Database's operator for each (SqlFragment.hs, `simpleOperator`): contains,
// contained by, overlap, strictly left of, strictly right of, does not extend
// to the left of, does not extend to the right of, adjacent to.
var pgrSimpleOperators = map[string]string{
	"cs":  "@>",
	"cd":  "<@",
	"ov":  "&&",
	"sl":  "<<",
	"sr":  ">>",
	"nxl": "&>",
	"nxr": "&<",
	"adj": "-|-",
}

// pgrFtsOperator reads a full-text operator and its optional configuration,
// `fts(english)` (QueryParams.hs, `pFts`: the configuration is an identifier of
// letters, digits, _, $ and spaces).
var pgrFtsOperator = regexp.MustCompile(`^(fts|plfts|phfts|wfts)(?:\(([A-Za-z0-9_ $]+)\))?$`)

// pgrFtsFunctions is the query function each full-text operator calls
// (SqlFragment.hs, `ftsOperator`).
var pgrFtsFunctions = map[string]string{
	"fts":   "to_tsquery",
	"plfts": "plainto_tsquery",
	"phfts": "phraseto_tsquery",
	"wfts":  "websearch_to_tsquery",
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
	for _, part := range pgrSplitGroup(v[1 : len(v)-1]) {
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
func pgrParseOrder(raw string) (string, error) { return pgrParseOrderAs("t", raw) }

// pgrParseOrderAs is pgrParseOrder over the columns of the table called alias.
func pgrParseOrderAs(alias, raw string) (string, error) {
	if strings.TrimSpace(raw) == "" {
		return "", nil
	}
	var parts []string
	for _, item := range pgrSplit(raw) {
		segments := strings.Split(strings.TrimSpace(item), ".")
		column, err := pgrParseColumn(segments[0])
		if err != nil {
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
		parts = append(parts, column.sql(alias)+" "+direction+nulls)
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
// items were written. Rendering a read that embeds adds the arguments of the
// embedded filters, so it is rendered once.
func (q *pgrRead) rowsSQL(table string) (string, error) {
	selected, err := q.fieldsSQL(q.items, "t")
	if err != nil {
		return "", err
	}
	return fmt.Sprintf(`SELECT coalesce(json_agg(x), '[]'::json)::text FROM (SELECT %s FROM %s AS "t"%s%s LIMIT %d OFFSET %d) AS x`,
		selected, pgrTable(table), q.whereSQL(), q.order, q.limit, q.offset), nil
}

// countSQL is how many rows the filters match, before the page. It is asked
// with countArgs.
func (q *pgrRead) countSQL(table string) string {
	return fmt.Sprintf(`SELECT count(*) FROM %s AS "t"%s`, pgrTable(table), q.whereSQL())
}
