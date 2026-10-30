package controllers

// miniship (miniship-cloud#1305, public ADR 0077): a read that brings related
// rows of another table, `select=*,other(*)`, as the Supabase SDK's
// postgrest-js writes it and PostgREST v12 answers it.
//
//	other(*)               the rows related through a foreign key: an object
//	                       for a many-to-one or one-to-one, an array for a
//	                       one-to-many or a many-to-many through a join table
//	alias:other(*)         under another key
//	other!fk_name(*)       the relationship named by a constraint, a column or
//	                       a join table, where more than one is possible
//	other!inner(*)         the row only when it has related rows
//	other.col=eq.1         a filter on the embedded table, and other.order,
//	other.limit, other.offset
//
// The relationships are the foreign keys of the `public` schema, read from the
// catalog in the same transaction as the read, as the caller's role, once for
// each read that embeds. Nothing is cached: a foreign key added a second ago is
// answered, and a read that embeds nothing pays nothing. The relationship is
// found by the rules of PostgREST's findRel (Plan.hs at v12.2.3) and its two
// refusals are PostgREST's: none found is PGRST200 (400), more than one is
// PGRST201 (300) with the candidates in details.
//
// The embedded table is read by the caller's role in the same transaction, so
// its grants and row rules decide which related rows arrive: a row the role
// may not read is absent, and a table it holds no grant on refuses the read.

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"

	"github.com/lib/pq"

	"github.com/prest/prest/v2/internal/pgrwire"
)

// pgrMaxEmbedDepth is how many tables a read may embed inside one another.
const pgrMaxEmbedDepth = 5

// pgrKey is a filter addressed to an embedded table, as the query string
// wrote it with the embedded name cut off.
type pgrKey struct{ key, value string }

// pgrEmbed is one embedded table of a select.
type pgrEmbed struct {
	name   string
	alias  string
	hint   string
	inner  bool
	items  []pgrItem
	keys   []pgrKey
	order  string
	limit  int
	offset int

	// Set when the read is planned, inside the transaction.
	rel *pgrRelation
	id  int
}

// label is the key the embedded rows are answered under.
func (e *pgrEmbed) label() string {
	if e.alias != "" {
		return e.alias
	}
	return e.name
}

// pgrParseEmbed reads `name!hint!inner(select)` after any alias. text starts
// at the name and open is the index of its first parenthesis.
func pgrParseEmbed(alias, text string, open, depth int) (*pgrEmbed, error) {
	if depth >= pgrMaxEmbedDepth {
		return nil, pgrRefuse(fmt.Sprintf("a read embeds at most %d tables inside one another", pgrMaxEmbedDepth))
	}
	if !strings.HasSuffix(text, ")") {
		return nil, pgrRefuse("an embedded table is written name(columns)")
	}
	embed := &pgrEmbed{alias: alias, limit: -1}
	head := pgrSplitOutside(text[:open], "!")
	name, err := pgrName(head[0], "table")
	if err != nil {
		return nil, err
	}
	embed.name = name
	for _, part := range head[1:] {
		part = strings.TrimSpace(part)
		switch part {
		case "inner":
			embed.inner = true
		case "left":
		default:
			if embed.hint != "" {
				return nil, pgrRefuse("an embedded table takes one hint: name!hint(columns)")
			}
			hint, err := pgrName(part, "hint")
			if err != nil {
				return nil, err
			}
			embed.hint = hint
		}
	}
	if body := strings.TrimSpace(text[open+1 : len(text)-1]); body != "" {
		items, err := pgrParseSelectList(body, depth+1)
		if err != nil {
			return nil, err
		}
		embed.items = items
	}
	return embed, nil
}

// embeds is every embedded table of a select, in the order they are written.
func pgrEmbedsOf(items []pgrItem) []*pgrEmbed {
	var out []*pgrEmbed
	for _, item := range items {
		if item.embed != nil {
			out = append(out, item.embed)
		}
	}
	return out
}

// pgrFindEmbed finds the embedded table a query-string key is addressed to:
// `other.name`, `other.order`, `other.child.name`. It answers that table and
// the rest of the key, or nil when the key's first segment names no embed.
func pgrFindEmbed(items []pgrItem, key string) (*pgrEmbed, string) {
	segments := pgrSplitOutside(key, ".")
	var current *pgrEmbed
	i := 0
	for ; i < len(segments)-1; i++ {
		var next *pgrEmbed
		for _, embed := range pgrEmbedsOf(items) {
			segment := strings.TrimSpace(segments[i])
			if unquoted, ok := pgrUnquote(segment); ok {
				segment = unquoted
			}
			if embed.label() == segment {
				next = embed
				break
			}
		}
		if next == nil {
			break
		}
		current, items = next, next.items
	}
	if current == nil {
		return nil, ""
	}
	return current, strings.Join(segments[i:], ".")
}

// address records a filter, an order, a limit or an offset of the embedded
// table, and refuses the ones that cannot be read. maxRows is the page ceiling
// a read's limit is held to.
func (e *pgrEmbed) address(local, value string, maxRows int) error {
	switch local {
	case "order":
		if _, err := pgrParseOrderAs("x", value); err != nil {
			return err
		}
		e.order = value
	case "limit":
		n, err := strconv.Atoi(value)
		if err != nil || n < 0 {
			return pgrRefuse("limit is a whole number")
		}
		if n > maxRows {
			return pgrTooManyRows(maxRows)
		}
		e.limit = n
	case "offset":
		n, err := strconv.Atoi(value)
		if err != nil || n < 0 {
			return pgrRefuse("offset is a whole number")
		}
		e.offset = n
	default:
		if err := (&pgrRead{alias: "x"}).filter(local, value); err != nil {
			return err
		}
		e.keys = append(e.keys, pgrKey{key: local, value: value})
	}
	return nil
}

// ---------------------------------------------------------------------------
// The relationships

type pgrCardinality int

const (
	pgrManyToOne pgrCardinality = iota
	pgrOneToMany
	pgrOneToOne
	pgrManyToMany
)

// pgrJunction is the join table of a many-to-many relationship. source pairs
// the origin's column with the join table's, target the foreign table's with
// the join table's.
type pgrJunction struct {
	table          string
	cons1, cons2   string
	source, target [][2]string
}

// pgrRelation is a relationship seen from one table, the origin. cols pairs
// the origin's column with the foreign table's; a many-to-many has none and
// goes through its junction.
type pgrRelation struct {
	table, foreign string
	isSelf         bool
	cardinality    pgrCardinality
	cons           string
	cols           [][2]string
	junction       *pgrJunction
}

func (r *pgrRelation) toOne() bool {
	return r.cardinality == pgrManyToOne || r.cardinality == pgrOneToOne
}

// pgrRelations are the relationships of the schema, by origin table.
type pgrRelations map[string][]*pgrRelation

// pgrForeignKeysSQL reads the foreign keys between tables of `public`, with
// whether each one is one-to-one (its columns are a primary key or a unique
// constraint of their table: PostgREST's allM2OandO2ORels) and the primary key
// of the table that holds it (a join table's keys must be part of it).
const pgrForeignKeysSQL = `
WITH pks_uniques_cols AS (
	SELECT connamespace, conrelid, jsonb_agg(column_info.cols) AS cols
	FROM pg_constraint
	JOIN LATERAL (
		SELECT array_agg(cols.attname ORDER BY cols.attnum) AS cols
		FROM (SELECT unnest(conkey) AS col) _
		JOIN pg_attribute cols ON cols.attrelid = conrelid AND cols.attnum = col
	) column_info ON TRUE
	WHERE contype IN ('p', 'u') AND connamespace = 'public'::regnamespace
	GROUP BY connamespace, conrelid
)
SELECT tab.relname::text, other.relname::text, traint.conname::text,
	column_info.src_cols, column_info.dst_cols,
	coalesce(column_info.cols IN (SELECT * FROM jsonb_array_elements(pks_uqs.cols)), false),
	ARRAY(SELECT a.attname::text FROM pg_constraint p
		JOIN pg_attribute a ON a.attrelid = p.conrelid AND a.attnum = ANY (p.conkey)
		WHERE p.contype = 'p' AND p.conrelid = traint.conrelid)
FROM pg_constraint traint
JOIN LATERAL (
	SELECT array_agg(cols.attname::text ORDER BY ord) AS src_cols,
		array_agg(refs.attname::text ORDER BY ord) AS dst_cols,
		jsonb_agg(cols.attname ORDER BY cols.attnum) AS cols
	FROM unnest(traint.conkey, traint.confkey) WITH ORDINALITY AS _(col, ref, ord)
	JOIN pg_attribute cols ON cols.attrelid = traint.conrelid AND cols.attnum = col
	JOIN pg_attribute refs ON refs.attrelid = traint.confrelid AND refs.attnum = ref
) AS column_info ON TRUE
JOIN pg_class tab ON tab.oid = traint.conrelid
JOIN pg_class other ON other.oid = traint.confrelid
LEFT JOIN pks_uniques_cols pks_uqs ON pks_uqs.connamespace = traint.connamespace AND pks_uqs.conrelid = traint.conrelid
WHERE traint.contype = 'f' AND traint.conparentid = 0
	AND traint.connamespace = 'public'::regnamespace
	AND other.relnamespace = 'public'::regnamespace
ORDER BY traint.conrelid, traint.conname`

type pgrForeignKey struct {
	table, foreign, cons string
	cols, foreignCols    []string
	oneToOne             bool
	primaryKey           []string
}

// pgrLoadRelations reads the schema's foreign keys through tx and turns them
// into the relationships seen from each table, as PostgREST's schema cache
// does: many-to-one and one-to-one from the key's own table, the inverse from
// the table it points at, and many-to-many through a join table with two keys
// whose columns are part of its primary key.
func pgrLoadRelations(ctx context.Context, tx *sql.Tx) (pgrRelations, error) {
	rows, err := tx.QueryContext(ctx, pgrForeignKeysSQL)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var keys []pgrForeignKey
	for rows.Next() {
		var key pgrForeignKey
		if err := rows.Scan(&key.table, &key.foreign, &key.cons, pq.Array(&key.cols),
			pq.Array(&key.foreignCols), &key.oneToOne, pq.Array(&key.primaryKey)); err != nil {
			return nil, err
		}
		keys = append(keys, key)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return pgrBuildRelations(keys), nil
}

func pgrPairs(left, right []string) [][2]string {
	out := make([][2]string, 0, len(left))
	for i := range left {
		out = append(out, [2]string{left[i], right[i]})
	}
	return out
}

func pgrBuildRelations(keys []pgrForeignKey) pgrRelations {
	relations := pgrRelations{}
	add := func(r *pgrRelation) { relations[r.table] = append(relations[r.table], r) }
	for _, key := range keys {
		self := key.table == key.foreign
		forward, inverse := pgrManyToOne, pgrOneToMany
		if key.oneToOne {
			forward, inverse = pgrOneToOne, pgrOneToOne
		}
		add(&pgrRelation{table: key.table, foreign: key.foreign, isSelf: self, cardinality: forward,
			cons: key.cons, cols: pgrPairs(key.cols, key.foreignCols)})
		add(&pgrRelation{table: key.foreign, foreign: key.table, isSelf: self, cardinality: inverse,
			cons: key.cons, cols: pgrPairs(key.foreignCols, key.cols)})
	}
	for _, first := range keys {
		for _, second := range keys {
			if first.table != second.table || first.cons == second.cons {
				continue
			}
			inKey := map[string]bool{}
			for _, name := range first.primaryKey {
				inKey[name] = true
			}
			part := true
			for _, name := range append(append([]string{}, first.cols...), second.cols...) {
				part = part && inKey[name]
			}
			if !part {
				continue
			}
			add(&pgrRelation{table: first.foreign, foreign: second.foreign, isSelf: first.foreign == second.foreign,
				cardinality: pgrManyToMany, junction: &pgrJunction{
					table: first.table, cons1: first.cons, cons2: second.cons,
					source: pgrPairs(first.foreignCols, first.cols),
					target: pgrPairs(second.foreignCols, second.cols),
				}})
		}
	}
	return relations
}

// find is PostgREST's findRel: the one relationship between origin and the
// target a select names, with the hint if it wrote one.
func (all pgrRelations) find(origin, target, hint string) (*pgrRelation, error) {
	var found []*pgrRelation
	for _, r := range all[origin] {
		if r.matches(target, hint) {
			found = append(found, r)
		}
	}
	switch len(found) {
	case 0:
		return nil, all.noRelationship(origin, target, hint)
	case 1:
		return found[0], nil
	}
	return nil, pgrAmbiguous(origin, target, found)
}

func (r *pgrRelation) singleColumn(name string, foreign bool) bool {
	if r.cardinality == pgrManyToMany || len(r.cols) != 1 {
		return false
	}
	if foreign {
		return r.cols[0][1] == name
	}
	return r.cols[0][0] == name
}

func (r *pgrRelation) matches(target, hint string) bool {
	matchConstraint := func(name string) bool { return r.cardinality != pgrManyToMany && r.cons == name }
	matchJunction := func(name string) bool { return r.cardinality == pgrManyToMany && r.junction.table == name }
	switch {
	case r.isSelf && hint == "":
		return (target == r.foreign && r.cardinality == pgrOneToMany) ||
			(r.singleColumn(target, false) && r.cardinality == pgrManyToOne)
	case r.isSelf:
		return target == r.foreign && r.cardinality == pgrOneToMany && r.singleColumn(hint, true)
	case hint == "":
		return target == r.foreign || matchConstraint(target) || r.singleColumn(target, false)
	}
	return target == r.foreign && (matchConstraint(hint) || r.singleColumn(hint, false) ||
		r.singleColumn(hint, true) || matchJunction(hint))
}

func (all pgrRelations) noRelationship(origin, target, hint string) error {
	details := fmt.Sprintf("Searched for a foreign key relationship between '%s' and '%s'", origin, target)
	if hint != "" {
		details += fmt.Sprintf(" using the hint '%s'", hint)
	}
	details += fmt.Sprintf(" in the schema '%s', but no matches were found.", pgrSchema)

	var suggestion string
	if _, known := all[origin]; known {
		var children []string
		for _, r := range all[origin] {
			children = append(children, r.foreign)
		}
		if best, ok := pgrClosest(target, children, pgrwire.RelationshipMinScore); ok {
			suggestion = fmt.Sprintf("Perhaps you meant '%s' instead of '%s'.", best, target)
		}
	} else {
		var parents []string
		for table := range all {
			parents = append(parents, table)
		}
		if best, ok := pgrClosest(origin, parents, pgrwire.RelationshipMinScore); ok {
			suggestion = fmt.Sprintf("Perhaps you meant '%s' instead of '%s'.", best, origin)
		}
	}
	return &pgrError{
		status:  400,
		code:    "PGRST200",
		message: fmt.Sprintf("Could not find a relationship between '%s' and '%s' in the schema cache", origin, target),
		details: details,
		hint:    suggestion,
	}
}

func pgrAmbiguous(origin, target string, found []*pgrRelation) error {
	var candidates []map[string]string
	var hints []string
	cols := func(pairs [][2]string, side int) string {
		names := make([]string, 0, len(pairs))
		for _, pair := range pairs {
			names = append(names, pair[side])
		}
		return "(" + strings.Join(names, ", ") + ")"
	}
	for _, r := range found {
		candidate := map[string]string{"embedding": r.table + " with " + r.foreign}
		switch r.cardinality {
		case pgrManyToMany:
			candidate["cardinality"] = "many-to-many"
			candidate["relationship"] = r.junction.table + " using " + r.junction.cons1 + cols(r.junction.source, 1) +
				" and " + r.junction.cons2 + cols(r.junction.target, 1)
			hints = append(hints, "'"+r.foreign+"!"+r.junction.table+"'")
		default:
			candidate["cardinality"] = map[pgrCardinality]string{pgrManyToOne: "many-to-one",
				pgrOneToMany: "one-to-many", pgrOneToOne: "one-to-one"}[r.cardinality]
			candidate["relationship"] = r.cons + " using " + r.table + cols(r.cols, 0) + " and " + r.foreign + cols(r.cols, 1)
			hints = append(hints, "'"+r.foreign+"!"+r.cons+"'")
		}
		candidates = append(candidates, candidate)
	}
	return &pgrError{
		status:      300,
		code:        "PGRST201",
		message:     fmt.Sprintf("Could not embed because more than one relationship was found for '%s' and '%s'", origin, target),
		detailsList: candidates,
		hint: fmt.Sprintf("Try changing '%s' to one of the following: %s. Find the desired relationship in the 'details' key.",
			target, strings.Join(hints, ", ")),
	}
}

// pgrClosest is the name Fuzzy.getOne of PostgREST 14 picks for want from
// names, at minScore (Error.hs; internal/pgrwire). A name that is want in
// another case is want itself and suggests nothing.
func pgrClosest(want string, names []string, minScore float64) (string, bool) {
	best := pgrwire.Suggest(names, want, minScore)
	if best == "" || strings.EqualFold(best, want) && best == want {
		return "", false
	}
	return best, true
}

// ---------------------------------------------------------------------------
// Planning and SQL

// hasEmbeds is whether the read embeds any table.
func (q *pgrRead) hasEmbeds() bool { return len(pgrEmbedsOf(q.items)) > 0 }

// plan resolves every embedded table of the read to a relationship, through
// tx, and adds to the read's own filters the rows an `!inner` embed requires.
// It is called once, before the SQL is rendered, and only for a read that
// embeds; the arguments the filters hold are counted after it, so that a count
// is asked with them alone.
func (q *pgrRead) plan(ctx context.Context, tx *sql.Tx, table string) error {
	if !q.hasEmbeds() {
		return nil
	}
	relations, err := pgrLoadRelations(ctx, tx)
	if err != nil {
		return err
	}
	next := 0
	if err := pgrResolve(relations, table, q.items, &next); err != nil {
		return err
	}
	for _, embed := range pgrEmbedsOf(q.items) {
		if !embed.inner {
			continue
		}
		predicate, err := q.embedSQL(embed, "t", true)
		if err != nil {
			return err
		}
		q.where = append(q.where, predicate)
	}
	q.whereArgs = len(q.args)
	return nil
}

func pgrResolve(relations pgrRelations, origin string, items []pgrItem, next *int) error {
	for _, embed := range pgrEmbedsOf(items) {
		rel, err := relations.find(origin, embed.name, embed.hint)
		if err != nil {
			return err
		}
		*next++
		embed.rel, embed.id = rel, *next
		if err := pgrResolve(relations, rel.foreign, embed.items, next); err != nil {
			return err
		}
	}
	return nil
}

// fieldsSQL is the items of a select as a select list over alias.
func (q *pgrRead) fieldsSQL(items []pgrItem, alias string) (string, error) {
	out := make([]string, 0, len(items))
	for _, item := range items {
		if item.field != nil {
			out = append(out, item.field.sql(alias))
			continue
		}
		if len(item.embed.items) == 0 {
			continue
		}
		text, err := q.embedSQL(item.embed, alias, false)
		if err != nil {
			return "", err
		}
		out = append(out, text)
	}
	return strings.Join(out, ", "), nil
}

// embedSQL is an embedded table as an item of its parent's select list: a
// correlated subquery answering the related rows as json under the embed's
// key. With exists it is instead the condition `!inner` puts on the parent's
// rows, which the embed's own limit and offset do not touch.
func (q *pgrRead) embedSQL(e *pgrEmbed, parent string, exists bool) (string, error) {
	alias := "e" + strconv.Itoa(e.id)
	sub := &pgrRead{root: q, alias: alias}
	for _, k := range e.keys {
		if err := sub.filter(k.key, k.value); err != nil {
			return "", err
		}
	}
	from, conditions := pgrLink(e.rel, alias, parent, e.id)
	conditions = append(conditions, sub.where...)
	for _, child := range pgrEmbedsOf(e.items) {
		if !child.inner {
			continue
		}
		text, err := q.embedSQL(child, alias, true)
		if err != nil {
			return "", err
		}
		conditions = append(conditions, text)
	}
	where := " WHERE " + strings.Join(conditions, " AND ")
	if exists {
		return "EXISTS (SELECT 1 FROM " + from + where + ")", nil
	}
	fields, err := q.fieldsSQL(e.items, alias)
	if err != nil {
		return "", err
	}
	order, err := pgrParseOrderAs(alias, e.order)
	if err != nil {
		return "", err
	}
	label := pgrQuote(e.label())
	if e.rel.toOne() {
		return fmt.Sprintf(`(SELECT row_to_json("x") FROM (SELECT %s FROM %s%s LIMIT 1) AS "x") AS %s`,
			fields, from, where, label), nil
	}
	limit := q.maxRows
	if e.limit >= 0 {
		limit = e.limit
	}
	return fmt.Sprintf(`(SELECT coalesce(json_agg("x"), '[]'::json) FROM (SELECT %s FROM %s%s%s LIMIT %d OFFSET %d) AS "x") AS %s`,
		fields, from, where, order, limit, e.offset, label), nil
}

// pgrLink is the FROM of an embedded table and the conditions tying it to its
// parent's row.
func pgrLink(r *pgrRelation, alias, parent string, id int) (string, []string) {
	if r.cardinality != pgrManyToMany {
		conditions := make([]string, 0, len(r.cols))
		for _, pair := range r.cols {
			conditions = append(conditions, pgrQuote(alias)+"."+pgrQuote(pair[1])+" = "+pgrQuote(parent)+"."+pgrQuote(pair[0]))
		}
		return pgrTable(r.foreign) + " AS " + pgrQuote(alias), conditions
	}
	join := "j" + strconv.Itoa(id)
	on := make([]string, 0, len(r.junction.target))
	for _, pair := range r.junction.target {
		on = append(on, pgrQuote(join)+"."+pgrQuote(pair[1])+" = "+pgrQuote(alias)+"."+pgrQuote(pair[0]))
	}
	conditions := make([]string, 0, len(r.junction.source))
	for _, pair := range r.junction.source {
		conditions = append(conditions, pgrQuote(join)+"."+pgrQuote(pair[1])+" = "+pgrQuote(parent)+"."+pgrQuote(pair[0]))
	}
	from := pgrTable(r.foreign) + " AS " + pgrQuote(alias) + " JOIN " + pgrTable(r.junction.table) +
		" AS " + pgrQuote(join) + " ON " + strings.Join(on, " AND ")
	return from, conditions
}
