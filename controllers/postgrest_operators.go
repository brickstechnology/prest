package controllers

// miniship (miniship-cloud#1887): the filter operators and the logic tree of
// PostgREST 14, parsed the way PostgREST parses them and turned into SQL with
// every value a bound parameter.
//
// The grammar is QueryParams.hs's (`pOpExpr`, `pListVal`, `pLogicTree`,
// `pLogicSingleVal`, `pQuotedValue`) at v14.18, the SQL is SqlFragment.hs's
// (`pgFmtFilter`, `pgFmtLogicTree`). The operators are:
//
//	quantifiable (take (any) and (all)): eq gt gte lt lte like ilike match imatch
//	single value:  neq cs cd ov sl sr nxl nxr adj
//	list:          in
//	is:            is (null, not_null, true, false, unknown, in any case)
//	distinct:      isdistinct
//	full text:     fts plfts phfts wfts, each with an optional (config)
//
// and every one takes `not.` in front. A refusal is PGRST100 with the message
// and details PostgREST gives for the same text: the position the parser failed
// at and what it expected there.

import (
	"fmt"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// pgrOpExpr is one parsed `[not.]operator[(quantifier)].value`.
type pgrOpExpr struct {
	negate bool
	// kind is "quant", "simple", "in", "is", "isdistinct" or "fts".
	kind string
	// op is the operator as written, without its quantifier or configuration.
	op string
	// quant is "", "any" or "all".
	quant string
	value string
	list  []string
	// is is the SQL keyword an `is` value stands for.
	is string
	// lang is the configuration of a full-text operator, "" when none was named.
	lang    string
	hasLang bool
}

// pgrQuantOperators are the operators that take (any) and (all), with the SQL
// each is (SqlFragment.hs, `quantOperator`). They are tried in this order, as
// QueryParams.hs's `quantOperator` does, so that gte is read before gt.
var pgrQuantOperators = []struct{ name, sql string }{
	{"eq", "="}, {"gte", ">="}, {"gt", ">"}, {"lte", "<="}, {"lt", "<"},
	{"like", "LIKE"}, {"ilike", "ILIKE"}, {"match", "~"}, {"imatch", "~*"},
}

// pgrSimpleOrder is the order QueryParams.hs's `simpleOperator` tries them in.
var pgrSimpleOrder = []string{"neq", "cs", "cd", "ov", "sl", "sr", "nxr", "nxl", "adj"}

// pgrSimpleSQL is the SQL of the single-value operators (SqlFragment.hs,
// `simpleOperator`).
var pgrSimpleSQL = map[string]string{
	"neq": "<>",
	"cs":  "@>",
	"cd":  "<@",
	"ov":  "&&",
	"sl":  "<<",
	"sr":  ">>",
	"nxl": "&>",
	"nxr": "&<",
	"adj": "-|-",
}

var pgrFtsOrder = []string{"fts", "plfts", "phfts", "wfts"}

// pgrFtsFunctions is the query function each full-text operator calls
// (SqlFragment.hs, `ftsOperator`).
var pgrFtsFunctions = map[string]string{
	"fts":   "to_tsquery",
	"plfts": "plainto_tsquery",
	"phfts": "phraseto_tsquery",
	"wfts":  "websearch_to_tsquery",
}

// pgrIsValues are the values of `is`, in the order QueryParams.hs's `pIsVal`
// tries them, and the keyword each stands for.
var pgrIsValues = []struct{ word, sql string }{
	{"null", "NULL"}, {"not_null", "NOT NULL"}, {"true", "TRUE"},
	{"false", "FALSE"}, {"unknown", "UNKNOWN"},
}

// ---------------------------------------------------------------------------
// The parser

// pgrParseFail is where the parser stopped and what it wanted there, in the
// words of Parsec, which PostgREST shows its caller unchanged.
type pgrParseFail struct {
	// at is a byte offset into the text parsed.
	at         int
	unexpected string
	expecting  string
}

// pgrOpParser reads the text of a filter or of a logic tree. logic is true for
// the values inside an and / or, which end at a comma or a parenthesis.
type pgrOpParser struct {
	src   string
	pos   int
	logic bool
}

func (p *pgrOpParser) rest() string { return p.src[p.pos:] }

// unexpectedAt names the character at byte offset i the way Parsec does.
func (p *pgrOpParser) unexpectedAt(i int) string {
	if i >= len(p.src) {
		return "unexpected end of input"
	}
	r, _ := utf8.DecodeRuneInString(p.src[i:])
	return "unexpected " + strconv.Quote(string(r))
}

func (p *pgrOpParser) fail(i int, expecting string) *pgrParseFail {
	return &pgrParseFail{at: i, unexpected: p.unexpectedAt(i), expecting: expecting}
}

// skipSpaces is Parsec's `lexeme`: the white space after a token.
func (p *pgrOpParser) skipSpaces() {
	for p.pos < len(p.src) {
		r, size := utf8.DecodeRuneInString(p.src[p.pos:])
		if !unicode.IsSpace(r) {
			return
		}
		p.pos += size
	}
}

// stringAt is Parsec's `string s` at offset start. When the text is not there
// it returns the failure Parsec reports: at the start of the string, naming
// the first character that differs.
func (p *pgrOpParser) stringAt(start int, s string) (bool, *pgrParseFail) {
	for i := 0; i < len(s); i++ {
		if start+i >= len(p.src) || p.src[start+i] != s[i] {
			return false, &pgrParseFail{at: start, unexpected: p.unexpectedAt(start + i), expecting: strconv.Quote(s)}
		}
	}
	return true, nil
}

// opExpr reads one `[not.]operator.value`.
func (p *pgrOpParser) opExpr() (pgrOpExpr, *pgrParseFail) {
	var e pgrOpExpr
	start := p.pos
	var notFail *pgrParseFail
	if ok, fail := p.stringAt(p.pos, "not"); ok {
		if p.pos+3 < len(p.src) && p.src[p.pos+3] == '.' {
			e.negate = true
			p.pos += 4
		} else {
			notFail = p.fail(p.pos+3, "delimiter (.)")
		}
	} else {
		notFail = fail
	}
	opStart := p.pos
	if err := p.operation(&e); err != nil {
		// The deepest failure wins; at the same place the expectations are
		// joined, which is how `"not" or operator (eq, gt, ...)` comes about.
		if notFail != nil && notFail.at > err.at {
			p.pos = start
			return e, notFail
		}
		if notFail != nil && notFail.at == err.at && !e.negate && err.at == opStart {
			err.unexpected = notFail.unexpected
			err.expecting = `"not" or ` + err.expecting
		}
		return e, err
	}
	return e, nil
}

// pgrOperatorLabel is what Parsec says a failed operator was expected to be.
const pgrOperatorLabel = "operator (eq, gt, ...)"

// operation reads the operator and its value, trying them in the order
// QueryParams.hs's `pOperation` does.
func (p *pgrOpParser) operation(e *pgrOpExpr) *pgrParseFail {
	from := p.pos
	s := p.rest()
	var deepest *pgrParseFail
	note := func(f *pgrParseFail) {
		if f != nil && (deepest == nil || f.at > deepest.at) {
			deepest = f
		}
	}

	// in.(…): once "in." is read, a failure is the list's.
	if ok, fail := p.stringAt(from, "in"); ok && strings.HasPrefix(s[2:], ".") {
		p.pos = from + 3
		return p.listValue(e)
	} else if ok {
		note(p.fail(from+2, "delimiter (.)"))
	} else {
		note(fail)
	}

	// is.<value>
	if ok, fail := p.stringAt(from, "is"); ok && strings.HasPrefix(s[2:], ".") {
		p.pos = from + 3
		return p.isValue(e)
	} else if ok {
		note(p.fail(from+2, "delimiter (.)"))
	} else {
		note(fail)
	}

	// isdistinct.<value>
	if ok, fail := p.stringAt(from, "isdistinct"); ok && strings.HasPrefix(s[10:], ".") {
		p.pos = from + 11
		e.kind, e.op = "isdistinct", "isdistinct"
		e.value = p.singleValue()
		return nil
	} else if ok {
		note(p.fail(from+10, "delimiter (.)"))
	} else {
		note(fail)
	}

	// fts, plfts, phfts and wfts, each with an optional (config).
	var matched string
	var ftsFail *pgrParseFail
	for _, name := range pgrFtsOrder {
		ok, fail := p.stringAt(from, name)
		if ok {
			matched = name
			break
		}
		if ftsFail == nil {
			ftsFail = fail
		}
	}
	if matched == "" {
		note(ftsFail)
	} else {
		at := from + len(matched)
		lang, hasLang, after, langFail := p.language(at)
		if langFail != nil {
			note(langFail)
		}
		if after < len(p.src) && p.src[after] == '.' {
			p.pos = after + 1
			e.kind, e.op, e.lang, e.hasLang = "fts", matched, lang, hasLang
			e.value = p.singleValue()
			return nil
		}
		note(p.fail(after, "delimiter (.)"))
	}

	// neq, cs, cd, ov, sl, sr, nxr, nxl and adj.
	matched = ""
	var simpleFail *pgrParseFail
	for _, name := range pgrSimpleOrder {
		ok, fail := p.stringAt(from, name)
		if ok {
			matched = name
			break
		}
		if simpleFail == nil {
			simpleFail = fail
		}
	}
	if matched == "" {
		note(simpleFail)
	} else {
		at := from + len(matched)
		if at < len(p.src) && p.src[at] == '.' {
			p.pos = at + 1
			e.kind, e.op = "simple", matched
			e.value = p.singleValue()
			return nil
		}
		note(p.fail(at, "delimiter (.)"))
	}

	// eq, gte, gt, lte, lt, like, ilike, match and imatch, with (any) or (all).
	matched = ""
	var quantFail *pgrParseFail
	for _, q := range pgrQuantOperators {
		ok, fail := p.stringAt(from, q.name)
		if ok {
			matched = q.name
			break
		}
		if quantFail == nil {
			quantFail = fail
		}
	}
	if matched == "" {
		note(quantFail)
	} else {
		at := from + len(matched)
		quant, after, quantFail := p.quantifier(at)
		if quantFail != nil {
			note(quantFail)
		}
		if after < len(p.src) && p.src[after] == '.' {
			p.pos = after + 1
			e.kind, e.op, e.quant = "quant", matched, quant
			e.value = p.singleValue()
			return nil
		}
		note(p.fail(after, "delimiter (.)"))
	}

	p.pos = from
	deepest.expecting = pgrOperatorLabel
	return deepest
}

// language reads the optional `(config)` of a full-text operator at offset at:
// an identifier of letters, digits, _, $ and spaces between parentheses.
func (p *pgrOpParser) language(at int) (lang string, has bool, after int, fail *pgrParseFail) {
	if at >= len(p.src) || p.src[at] != '(' {
		return "", false, at, nil
	}
	i := at + 1
	for i < len(p.src) && pgrIdentifierChar(p.src[i]) {
		i++
	}
	if i == at+1 {
		return "", false, at, p.fail(i, pgrOperatorLabel)
	}
	if i >= len(p.src) || p.src[i] != ')' {
		return "", false, at, p.fail(i, pgrOperatorLabel)
	}
	return strings.TrimSpace(p.src[at+1 : i]), true, i + 1, nil
}

func pgrIdentifierChar(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == ' ' || c == '$'
}

// quantifier reads the optional `(any)` or `(all)` of a quantifiable operator.
func (p *pgrOpParser) quantifier(at int) (quant string, after int, fail *pgrParseFail) {
	if at >= len(p.src) || p.src[at] != '(' {
		return "", at, nil
	}
	for _, word := range []string{"any", "all"} {
		end := at + 1 + len(word)
		if strings.HasPrefix(p.src[at+1:], word) && end < len(p.src) && p.src[end] == ')' {
			return word, end + 1, nil
		}
	}
	return "", at, p.fail(at+1, pgrOperatorLabel)
}

// isValue reads the value of `is`, in any case. What follows it is not read:
// PostgREST stops there too.
func (p *pgrOpParser) isValue(e *pgrOpExpr) *pgrParseFail {
	e.kind, e.op = "is", "is"
	var deepest *pgrParseFail
	for _, v := range pgrIsValues {
		n := 0
		for n < len(v.word) && p.pos+n < len(p.src) && strings.EqualFold(p.src[p.pos+n:p.pos+n+1], v.word[n:n+1]) {
			n++
		}
		if n == len(v.word) {
			e.is = v.sql
			p.pos += n
			return nil
		}
		if f := p.fail(p.pos+n, "isVal: (null, not_null, true, false, unknown)"); deepest == nil || f.at > deepest.at {
			deepest = f
		}
	}
	return deepest
}

// listValue reads `(a,b,"c,d")`, the value of `in`: `pListVal`. An element is a
// double-quoted run, with \ escaping the next character, or the text up to the
// next comma or closing parenthesis. The empty list `()` is one empty element.
func (p *pgrOpParser) listValue(e *pgrOpExpr) *pgrParseFail {
	e.kind, e.op = "in", "in"
	if p.pos >= len(p.src) || p.src[p.pos] != '(' {
		return p.fail(p.pos, `"("`)
	}
	p.pos++
	p.skipSpaces()
	for {
		e.list = append(e.list, p.listElement())
		if p.pos < len(p.src) && p.src[p.pos] == ',' {
			p.pos++
			continue
		}
		break
	}
	if p.pos >= len(p.src) || p.src[p.pos] != ')' {
		return p.fail(p.pos, `"," or ")"`)
	}
	p.pos++
	p.skipSpaces()
	return nil
}

// quoted reads a double-quoted run at offset at. ok is false when it is not
// closed.
func (p *pgrOpParser) quoted(at int) (value string, end int, ok bool) {
	if at >= len(p.src) || p.src[at] != '"' {
		return "", at, false
	}
	var out strings.Builder
	i := at + 1
	for i < len(p.src) {
		switch c := p.src[i]; c {
		case '"':
			return out.String(), i + 1, true
		case '\\':
			if i+1 >= len(p.src) {
				return "", at, false
			}
			_, size := utf8.DecodeRuneInString(p.src[i+1:])
			out.WriteString(p.src[i+1 : i+1+size])
			i += 1 + size
		default:
			out.WriteByte(c)
			i++
		}
	}
	return "", at, false
}

// until is the text from offset at up to the first byte in stops.
func (p *pgrOpParser) until(at int, stops string) (string, int) {
	end := at
	for end < len(p.src) && !strings.ContainsRune(stops, rune(p.src[end])) {
		end++
	}
	return p.src[at:end], end
}

// untilUnquoted is until, except that a double-quoted run is passed over whole,
// so a column named "a.b" or "a,b" is one name.
func (p *pgrOpParser) untilUnquoted(at int, stops string) (string, int) {
	end := at
	for end < len(p.src) && !strings.ContainsRune(stops, rune(p.src[end])) {
		if p.src[end] == '"' {
			if closed := pgrQuotedEnd(p.src, end); closed > 0 {
				end = closed
				continue
			}
		}
		end++
	}
	return p.src[at:end], end
}

func (p *pgrOpParser) listElement() string {
	if value, end, ok := p.quoted(p.pos); ok {
		// notFollowedBy (noneOf ",)"): the quote closes the element.
		if end >= len(p.src) || p.src[end] == ',' || p.src[end] == ')' {
			p.pos = end
			return value
		}
	}
	text, end := p.until(p.pos, ",)")
	p.pos = end
	return text
}

// singleValue reads the value of an operator that takes one: all the rest of a
// filter, and inside an and / or a quoted run, a `{…}` run kept as written, or
// the text up to the next comma or closing parenthesis (`pLogicSingleVal`).
func (p *pgrOpParser) singleValue() string {
	if !p.logic {
		value := p.rest()
		p.pos = len(p.src)
		return value
	}
	if value, end, ok := p.quoted(p.pos); ok {
		if end >= len(p.src) || p.src[end] == ',' || p.src[end] == ')' {
			p.pos = end
			return value
		}
	}
	if p.pos < len(p.src) && p.src[p.pos] == '{' {
		end := p.pos + 1
		for end < len(p.src) && p.src[end] != '{' && p.src[end] != '}' {
			end++
		}
		if end < len(p.src) && p.src[end] == '}' {
			value := p.src[p.pos : end+1]
			p.pos = end + 1
			return value
		}
	}
	text, end := p.until(p.pos, ",)")
	p.pos = end
	return text
}

// ---------------------------------------------------------------------------
// The logic tree

// pgrLogic is a node of an and / or tree: a group, or one filter of a column.
type pgrLogic struct {
	not      bool
	or       bool
	children []*pgrLogic

	leaf   bool
	column string
	expr   pgrOpExpr
}

const pgrFieldLabel = "field name (* or [a..z0..9_$]), negation operator (not) or logic operator (and, or)"

// node reads one filter or one group: a filter first, as `pLogicTree` does, and
// a group when it is not a filter.
func (p *pgrOpParser) node() (*pgrLogic, error) {
	start := p.pos
	leaf, filterFail, hard := p.logicFilter()
	if hard != nil {
		return nil, hard
	}
	if leaf != nil {
		return leaf, nil
	}
	p.pos = start

	group := &pgrLogic{}
	consumed := false
	if strings.HasPrefix(p.rest(), "not.") {
		group.not, consumed = true, true
		p.pos += 4
	}
	switch {
	case strings.HasPrefix(p.rest(), "and"):
		p.pos += 3
		consumed = true
	case strings.HasPrefix(p.rest(), "or"):
		group.or = true
		p.pos += 2
		consumed = true
	default:
		if !consumed {
			// Nothing was read, so the two failures at the deeper place are one.
			fail := filterFail
			if fail == nil || fail.at <= start {
				fail = p.fail(start, pgrFieldLabel)
			}
			return nil, fail
		}
		return nil, p.fail(p.pos, "logic operator (and, or)")
	}
	if p.pos >= len(p.src) || p.src[p.pos] != '(' {
		return nil, p.fail(p.pos, `"("`)
	}
	p.pos++
	p.skipSpaces()
	for {
		child, err := p.node()
		if err != nil {
			return nil, err
		}
		group.children = append(group.children, child)
		p.skipSpaces()
		if p.pos < len(p.src) && p.src[p.pos] == ',' {
			p.pos++
			p.skipSpaces()
			continue
		}
		break
	}
	if p.pos >= len(p.src) || p.src[p.pos] != ')' {
		return nil, p.fail(p.pos, `"," or ")"`)
	}
	p.pos++
	p.skipSpaces()
	return group, nil
}

// logicFilter reads `column.operator.value`. A text that is not a filter at
// all is a failure to fall back from (fail); a column that is not a plain name
// is a refusal of its own (hard).
func (p *pgrOpParser) logicFilter() (leaf *pgrLogic, fail *pgrParseFail, hard error) {
	field, end := p.untilUnquoted(p.pos, ".(),")
	if end >= len(p.src) || p.src[end] != '.' || strings.TrimSpace(field) == "" {
		return nil, p.fail(end, "delimiter (.)"), nil
	}
	if _, err := pgrParseColumn(field); err != nil {
		return nil, nil, err
	}
	p.pos = end + 1
	expr, opFail := p.opExpr()
	if opFail != nil {
		return nil, opFail, nil
	}
	return &pgrLogic{leaf: true, column: strings.TrimSpace(field), expr: expr}, nil, nil
}

// ---------------------------------------------------------------------------
// Refusals

// pgrParseRefusal is PGRST100 as PostgREST words a query string it cannot
// parse: the message is the name of what was parsed and the position, the
// details what was found and what was expected.
func pgrParseRefusal(what, text string, fail *pgrParseFail, src string) error {
	column := utf8.RuneCountInString(src[:min(fail.at, len(src))]) + 1
	details := fail.unexpected
	if fail.expecting != "" {
		details += " expecting " + fail.expecting
	}
	return &pgrError{
		status:  400,
		code:    "PGRST100",
		message: fmt.Sprintf("%s (line 1, column %d)", strconv.Quote(fmt.Sprintf("failed to parse %s (%s)", what, text)), column),
		details: details,
	}
}

// ---------------------------------------------------------------------------
// The SQL

// parseFilter reads the value of a column's filter.
func pgrParseFilter(expr string) (pgrOpExpr, error) {
	p := &pgrOpParser{src: expr}
	e, fail := p.opExpr()
	if fail != nil {
		return e, pgrParseRefusal("filter", expr, fail, expr)
	}
	return e, nil
}

// pgrArrayLiteral is the Postgres array literal of a list, every element double
// quoted (SqlFragment.hs, `pgBuildArrayLiteral`): sent as one parameter, the
// Database reads it as the type the column needs.
func pgrArrayLiteral(items []string) string {
	var b strings.Builder
	b.WriteByte('{')
	for i, item := range items {
		if i > 0 {
			b.WriteByte(',')
		}
		if n := strings.IndexByte(item, 0); n >= 0 {
			item = item[:n]
		}
		item = strings.ReplaceAll(item, `\`, `\\`)
		item = strings.ReplaceAll(item, `"`, `\"`)
		b.WriteByte('"')
		b.WriteString(item)
		b.WriteByte('"')
	}
	b.WriteByte('}')
	return b.String()
}

// sql is the predicate for column c (already quoted).
func (e pgrOpExpr) sql(q *pgrRead, c string) string {
	var text string
	switch e.kind {
	case "quant":
		var symbol string
		for _, o := range pgrQuantOperators {
			if o.name == e.op {
				symbol = o.sql
			}
		}
		value := e.value
		if e.op == "like" || e.op == "ilike" {
			value = strings.ReplaceAll(value, "*", "%")
		}
		hole := q.arg(value)
		switch e.quant {
		case "any":
			hole = "ANY(" + hole + ")"
		case "all":
			hole = "ALL(" + hole + ")"
		}
		text = c + " " + symbol + " " + hole
	case "simple":
		// PostgREST hands the value to the Database as an unknown literal, so
		// `{a,b}`, a json value and a range literal are read by the column's
		// own type; so is a parameter. A value that does not parse is the
		// Database's 22P02, and an operator the type lacks is its 42883.
		text = c + " " + pgrSimpleSQL[e.op] + " " + q.arg(e.value)
	case "is":
		text = c + " IS " + e.is
	case "isdistinct":
		text = c + " IS DISTINCT FROM " + q.arg(e.value)
	case "in":
		// `= ANY(array)`: one parameter however long the list, and the empty
		// list is an array of nothing, as `[""]` is for PostgREST.
		if len(e.list) == 1 && e.list[0] == "" {
			text = c + " = ANY('{}')"
		} else {
			text = c + " = ANY(" + q.arg(pgrArrayLiteral(e.list)) + ")"
		}
	case "fts":
		call := pgrFtsFunctions[e.op] + "("
		if e.hasLang {
			call += q.arg(e.lang) + ", "
		}
		text = c + " @@ " + call + q.arg(e.value) + ")"
	}
	if e.negate {
		text = "NOT (" + text + ")"
	}
	return text
}

// sql renders the tree, a group in parentheses.
func (n *pgrLogic) sql(q *pgrRead) (string, error) {
	if n.leaf {
		column, err := pgrParseColumn(n.column)
		if err != nil {
			return "", err
		}
		return n.expr.sql(q, column.sql(q.tableAlias())), nil
	}
	join := " AND "
	if n.or {
		join = " OR "
	}
	parts := make([]string, 0, len(n.children))
	for _, child := range n.children {
		part, err := child.sql(q)
		if err != nil {
			return "", err
		}
		parts = append(parts, part)
	}
	text := "(" + strings.Join(parts, join) + ")"
	if n.not {
		text = "NOT " + text
	}
	return text, nil
}

// logicTree reads the value of an `or`, `and`, `not.or` or `not.and` key.
// PostgREST parses the key's operator in front of the value, as
// `not.or(a.eq.1,b.eq.2)`, so a failure is placed in that text.
func (q *pgrRead) logicTree(op, value string) (string, error) {
	src := op + value
	p := &pgrOpParser{src: src, logic: true}
	node, err := p.node()
	if err != nil {
		if fail, ok := err.(*pgrParseFail); ok {
			return "", pgrParseRefusal("logic tree", value, fail, src)
		}
		return "", err
	}
	return node.sql(q)
}

func (f *pgrParseFail) Error() string { return f.unexpected + " expecting " + f.expecting }
