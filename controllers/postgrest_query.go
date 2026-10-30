package controllers

// miniship (miniship-cloud#1302, public ADR 0077): PostgREST's query language
// for a read, as the Supabase SDK's database client writes it. Reads only:
// the columns named, every filter operator of PostgREST 14 (#1887,
// postgrest_operators.go), each of them under not., an or=( … ) group, order,
// limit and offset.
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
	"unicode/utf8"

	"github.com/lib/pq"
)

// pgrSchema is the one schema the address serves, as the table read serves.
const pgrSchema = servedSchema

// pgrIdent is a plain Postgres name. A name in double quotes is read by pgrName
// instead; a JSON path, a cast and an alias are read by postgrest_select.go
// around a name that passed one or the other.
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
	// challenge is the WWW-Authenticate value of a 401; empty is "Bearer".
	challenge string
}

func (e *pgrError) Error() string { return e.message }

// pgrRefuse is a refusal of the query itself, PGRST100 as PostgREST names a
// query string it cannot parse.
func pgrRefuse(message string) error {
	return &pgrError{status: 400, code: "PGRST100", message: message}
}

// pgrBadSchema is PGRST106: a schema the address does not serve. PostgREST 14
// names the schema and, in the hint, the ones it exposes (Error.hs,
// UnacceptableSchema).
func pgrBadSchema(profile string) error {
	return &pgrError{
		status:  406,
		code:    "PGRST106",
		message: "Invalid schema: " + profile,
		hint:    "Only the following schemas are exposed: " + pgrSchema,
	}
}

// pgrBadContentType is a body in a Content-Type rest does not read. PostgREST
// 14 answers it as an invalid body, 400 PGRST102 (ApiRequest/Payload.hs).
func pgrBadContentType(media string) error {
	return &pgrError{status: 400, code: "PGRST102", message: "Content-Type not acceptable: " + media}
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

// pgrQuotedEnd is the offset just past the double-quoted run that opens at
// s[i], or -1 when it is not closed. A backslash escapes the next character
// (QueryParams.hs:688, pQuotedValue).
func pgrQuotedEnd(s string, i int) int {
	for j := i + 1; j < len(s); j++ {
		switch s[j] {
		case '\\':
			j++
		case '"':
			return j + 1
		}
	}
	return -1
}

// pgrUnquote reads s as one whole double-quoted run: the name inside, with
// each backslash taken off its character. ok is false when s is anything else.
func pgrUnquote(s string) (string, bool) {
	if len(s) < 2 || s[0] != '"' || pgrQuotedEnd(s, 0) != len(s) {
		return "", false
	}
	var out strings.Builder
	for i := 1; i < len(s)-1; i++ {
		if s[i] == '\\' {
			i++
		}
		out.WriteByte(s[i])
	}
	return out.String(), true
}

// pgrIndexOutside is the offset of the first sub in s that is not inside a
// double-quoted run, or -1. A run that is never closed hides the rest of s.
func pgrIndexOutside(s, sub string) int {
	for i := 0; i < len(s); {
		if s[i] == '"' {
			end := pgrQuotedEnd(s, i)
			if end < 0 {
				return -1
			}
			i = end
			continue
		}
		if strings.HasPrefix(s[i:], sub) {
			return i
		}
		i++
	}
	return -1
}

// pgrCutOutside is strings.Cut for a separator outside double quotes.
func pgrCutOutside(s, sep string) (before, after string, found bool) {
	if i := pgrIndexOutside(s, sep); i >= 0 {
		return s[:i], s[i+len(sep):], true
	}
	return s, "", false
}

// pgrSplitOutside is strings.Split for a separator outside double quotes.
func pgrSplitOutside(s, sep string) []string {
	var out []string
	for {
		before, after, found := pgrCutOutside(s, sep)
		out = append(out, before)
		if !found {
			return out
		}
		s = after
	}
}

// pgrName reads a column, an alias, a table or a hint as PostgREST 14 does
// (QueryParams.hs:353, pFieldName): a double-quoted name of any characters,
// kept as written with each backslash taken off its character, or a plain
// name. Spaces outside the quotes are dropped. Nothing but the name leaves it,
// and it is quoted with pgrQuote wherever it becomes SQL, so a `"` or a `;`
// inside it is text, never syntax. A name Postgres cannot hold is refused
// here: the empty one, and one with a NUL byte, which pq.QuoteIdentifier would
// cut short.
func pgrName(raw, what string) (string, error) {
	raw = strings.TrimSpace(raw)
	if !strings.HasPrefix(raw, `"`) {
		if err := pgrCheckName(raw, what); err != nil {
			return "", err
		}
		return raw, nil
	}
	name, ok := pgrUnquote(raw)
	if !ok {
		return "", pgrRefuse(fmt.Sprintf(`a quoted %s is closed by a " and nothing follows it`, what))
	}
	if name == "" || strings.IndexByte(name, 0) >= 0 || !utf8.ValidString(name) {
		return "", pgrRefuse(fmt.Sprintf("a quoted %s is not empty and holds no NUL byte", what))
	}
	return name, nil
}

// pgrSplit splits s on commas outside parentheses and outside double quotes.
// The quotes stay, so the parts are read again by pgrName.
func pgrSplit(s string) []string {
	var out []string
	start, depth := 0, 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '"':
			end := pgrQuotedEnd(s, i)
			if end < 0 {
				return append(out, s[start:])
			}
			i = end - 1
		case '(':
			depth++
		case ')':
			depth--
		case ',':
			if depth == 0 {
				out = append(out, s[start:i])
				start = i + 1
			}
		}
	}
	return append(out, s[start:])
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
	case "or", "and", "not.or", "not.and":
		predicate, err = q.logicTree(key, value)
	default:
		predicate, err = q.predicate(key, value)
	}
	if err != nil {
		return err
	}
	q.where = append(q.where, predicate)
	return nil
}

// predicate turns one filter, `op.value` or `not.op.value` on column, into SQL
// (postgrest_operators.go).
func (q *pgrRead) predicate(column, expr string) (string, error) {
	parsed, err := pgrParseColumn(column)
	if err != nil {
		return "", err
	}
	parsedExpr, err := pgrParseFilter(expr)
	if err != nil {
		return "", err
	}
	return parsedExpr.sql(q, parsed.sql(q.tableAlias())), nil
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
		segments := pgrSplitOutside(strings.TrimSpace(item), ".")
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
	return q.rowsSQLAs(table, pgrMedia{})
}

// pageSQL is the page's rows, before they are aggregated into a body.
func (q *pgrRead) pageSQL(table string) (string, error) {
	selected, err := q.fieldsSQL(q.items, "t")
	if err != nil {
		return "", err
	}
	return fmt.Sprintf(`SELECT %s FROM %s AS "t"%s%s LIMIT %d OFFSET %d`,
		selected, pgrTable(table), q.whereSQL(), q.order, q.limit, q.offset), nil
}

// rowsSQLAs is the page as the body media asks for it (postgrest_media.go).
func (q *pgrRead) rowsSQLAs(table string, media pgrMedia) (string, error) {
	page, err := q.pageSQL(table)
	if err != nil {
		return "", err
	}
	return media.aggregate(page, "x", false), nil
}

// countSQL is how many rows the filters match, before the page. It is asked
// with countArgs.
func (q *pgrRead) countSQL(table string) string {
	return fmt.Sprintf(`SELECT count(*) FROM %s AS "t"%s`, pgrTable(table), q.whereSQL())
}
