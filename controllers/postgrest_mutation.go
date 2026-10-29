package controllers

// miniship (miniship-cloud#1303, public ADR 0077): the SQL of a write on
// /rest/v1/{table}, in PostgREST's shape. A body is never spliced into SQL: it
// is one JSON parameter, turned into rows by json_to_recordset against the
// column types the Database itself reports, so a value is read as its column's
// type reads it (an array column takes a JSON array, a json column keeps its
// text). A name that reaches SQL text passed pgrIdent, or came out of the
// catalog, and is quoted.
//
// Every write is one statement: a data-modifying WITH whose RETURNING rows are
// counted, and shown only when the caller asked for them back.

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"strings"

	"github.com/lib/pq"
)

// pgrTableColumn is one column of the table written, as the catalog reports it.
type pgrTableColumn struct {
	name string
	// typ is format_type's spelling, the type json_to_recordset reads the
	// column's value as.
	typ string
	// def is the column's default expression, or "" when it has none or is
	// generated.
	def string
}

// pgrLoadColumns asks the catalog for every column of the table, in order. A
// table that is not there is 42P01, as a read of it is.
func pgrLoadColumns(ctx context.Context, tx *sql.Tx, table string) ([]pgrTableColumn, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT a.attname::text, format_type(a.atttypid, a.atttypmod),
		       CASE WHEN a.attgenerated = '' THEN coalesce(pg_get_expr(d.adbin, d.adrelid), '') ELSE '' END
		FROM pg_catalog.pg_attribute a
		LEFT JOIN pg_catalog.pg_attrdef d ON d.adrelid = a.attrelid AND d.adnum = a.attnum AND a.atthasdef
		WHERE a.attrelid = $1::regclass AND a.attnum > 0 AND NOT a.attisdropped
		ORDER BY a.attnum`, pgrTable(table))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var columns []pgrTableColumn
	for rows.Next() {
		var column pgrTableColumn
		if err := rows.Scan(&column.name, &column.typ, &column.def); err != nil {
			return nil, err
		}
		columns = append(columns, column)
	}
	return columns, rows.Err()
}

// pgrLoadPrimaryKey is the columns of the table's primary key, in key order,
// or none when it has no primary key.
func pgrLoadPrimaryKey(ctx context.Context, tx *sql.Tx, table string) ([]string, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT a.attname::text
		FROM pg_catalog.pg_index i
		JOIN pg_catalog.pg_attribute a ON a.attrelid = i.indrelid AND a.attnum = ANY (i.indkey)
		WHERE i.indrelid = $1::regclass AND i.indisprimary
		ORDER BY array_position(i.indkey::int2[], a.attnum)`, pgrTable(table))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var key []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		key = append(key, name)
	}
	return key, rows.Err()
}

// pgrBody is a write's JSON body, looked at but not interpreted: whether it is
// an array, how many rows it holds, and the keys its objects use, in the
// order they were first seen.
type pgrBody struct {
	raw     []byte
	isArray bool
	rows    int
	keys    []string
}

// pgrParseBody checks that raw is a JSON object, or an array of objects, and
// reads its keys. An array of more than maxRows objects is refused before
// the Database is asked anything: the write quota (MS104).
func pgrParseBody(raw []byte, maxRows int) (*pgrBody, error) {
	invalid := &pgrError{status: 400, code: "PGRST102", message: "Empty or invalid json",
		details: "the body of a write is a JSON object, or an array of JSON objects"}
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" {
		return nil, invalid
	}
	body := &pgrBody{raw: raw}
	var items []json.RawMessage
	switch trimmed[0] {
	case '{':
		items = []json.RawMessage{json.RawMessage(trimmed)}
	case '[':
		body.isArray = true
		if err := json.Unmarshal([]byte(trimmed), &items); err != nil {
			return nil, invalid
		}
	default:
		return nil, invalid
	}
	if len(items) > maxRows {
		return nil, pgrWriteQuota(len(items), maxRows, "the body holds")
	}
	seen := map[string]bool{}
	for _, item := range items {
		keys, err := pgrObjectKeys(item)
		if err != nil {
			return nil, invalid
		}
		for _, key := range keys {
			if !seen[key] {
				seen[key] = true
				body.keys = append(body.keys, key)
			}
		}
	}
	body.rows = len(items)
	return body, nil
}

// pgrObjectKeys is the keys of a JSON object, in the order it wrote them.
func pgrObjectKeys(raw json.RawMessage) ([]string, error) {
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	if token, err := decoder.Token(); err != nil || token != json.Delim('{') {
		return nil, fmt.Errorf("not an object")
	}
	var keys []string
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return nil, err
		}
		key, _ := token.(string)
		keys = append(keys, key)
		var skipped json.RawMessage
		if err := decoder.Decode(&skipped); err != nil {
			return nil, err
		}
	}
	if _, err := decoder.Token(); err != nil && err != io.EOF {
		return nil, err
	}
	return keys, nil
}

// pgrWriteQuota is the refusal of a write over the row quota. The quota is the
// page a read may return (MaxPageSize): a write changes no more rows than a
// read shows. Over it the write is refused whole and changes nothing.
func pgrWriteQuota(rows, quota int, subject string) error {
	return &pgrError{
		status:  400,
		code:    "MS104",
		message: "the write changes more rows than rest allows at once",
		details: fmt.Sprintf("%s %d rows; rest changes at most %d rows a write", subject, rows, quota),
		hint:    fmt.Sprintf("write %d or fewer rows at a time, or add a filter that matches fewer", quota),
	}
}

// pgrSchemaColumn finds the column called name in the table, or says what
// PostgREST says of a body that names one the table lacks.
func pgrSchemaColumn(table string, columns []pgrTableColumn, name string) (pgrTableColumn, error) {
	for _, column := range columns {
		if column.name == name {
			return column, nil
		}
	}
	return pgrTableColumn{}, &pgrError{
		status:  400,
		code:    "PGRST204",
		message: fmt.Sprintf("Could not find the '%s' column of '%s' in the schema cache", name, table),
	}
}

// pgrWriteKind is which verb a write is.
type pgrWriteKind int

const (
	pgrInsert pgrWriteKind = iota
	pgrUpdate
	pgrDelete
)

// pgrMutation is one write, planned: the columns it writes, what it returns,
// and how a conflict is settled.
type pgrMutation struct {
	kind   pgrWriteKind
	table  string
	schema []pgrTableColumn
	// written are the columns the body sets, in the order the body used them.
	written []pgrTableColumn
	body    *pgrBody
	// bodyArg is the placeholder number of the body, after the filters'.
	bodyArg int
	where   string
	// conflict is on_conflict's columns, or the primary key's, and merge says
	// whether a conflict updates the row (merge-duplicates) or leaves it
	// (ignore-duplicates).
	conflict []string
	upsert   bool
	merge    bool
	// defaults applies the columns' defaults to the keys a row omits
	// (Prefer: missing=default).
	defaults bool
	// representation is whether the changed rows come back.
	representation bool
	// headersOnly is Prefer: return=headers-only. An insert then answers a
	// Location naming the row's primary key (miniship-cloud#1888), and locate
	// are those columns, in the table's column order.
	headersOnly bool
	locate      []string
	// selected are the columns returned; order is the ORDER BY of the rows
	// returned, built by pgrParseOrder.
	selected []string
	order    string
	// tracksInserted is whether the statement tells inserted rows from merged
	// ones: only a merge-duplicates upsert's status depends on it.
	tracksInserted bool
	quota          int
	// media is how the rows come back (postgrest_media.go).
	media pgrMedia
}

func (m *pgrMutation) jsonType() string {
	if m.kind == pgrInsert && m.defaults && m.hasDefaults() {
		return "jsonb"
	}
	return "json"
}

func (m *pgrMutation) hasDefaults() bool {
	for _, column := range m.written {
		if column.def != "" {
			return true
		}
	}
	return false
}

// typed is the column definition list json_to_recordset reads the body with.
func (m *pgrMutation) typed() string {
	parts := make([]string, 0, len(m.written))
	for _, column := range m.written {
		parts = append(parts, pgrQuote(column.name)+" "+column.typ)
	}
	return strings.Join(parts, ", ")
}

func (m *pgrMutation) quotedWritten() []string {
	names := make([]string, 0, len(m.written))
	for _, column := range m.written {
		names = append(names, pgrQuote(column.name))
	}
	return names
}

// rowsExpression is the body as a JSON array, the columns' defaults laid under
// each row when Prefer: missing=default asked for them. PostgREST lays the
// defaults under an object with ||, so a key the row names, null included,
// keeps the row's own value.
func (m *pgrMutation) rowsExpression() string {
	jt := m.jsonType()
	array := "pgrst_payload.json_data"
	if !m.body.isArray {
		array = jt + "_build_array(pgrst_payload.json_data)"
	}
	if jt != "jsonb" {
		return array
	}
	var defaults []string
	for _, column := range m.written {
		if column.def != "" {
			defaults = append(defaults, pq.QuoteLiteral(column.name)+", "+column.def)
		}
	}
	if len(defaults) == 0 {
		return array
	}
	return fmt.Sprintf("(SELECT jsonb_agg(jsonb_build_object(%s) || elem) FROM jsonb_array_elements(%s) elem)",
		strings.Join(defaults, ", "), array)
}

// returning is the RETURNING list: every column when the rows come back (the
// order and the select are applied to them afterwards), one constant when they
// do not, and the flag that tells an inserted row from a merged one when the
// status needs it.
func (m *pgrMutation) returning() string {
	var parts []string
	if m.representation {
		for _, column := range m.schema {
			parts = append(parts, `"t".`+pgrQuote(column.name))
		}
	} else {
		parts = append(parts, `1 AS "pgrst_n"`)
		if m.headersOnly && m.kind == pgrInsert {
			for i, name := range m.locate {
				parts = append(parts, fmt.Sprintf(`"t".%s AS "pgrst_loc_%d"`, pgrQuote(name), i))
			}
		}
	}
	if m.tracksInserted {
		parts = append(parts, `("t".xmax = 0) AS "pgrst_inserted"`)
	}
	return strings.Join(parts, ", ")
}

func (m *pgrMutation) statement() string {
	switch m.kind {
	case pgrInsert:
		return m.insertSQL()
	case pgrUpdate:
		return m.updateSQL()
	default:
		return fmt.Sprintf(`DELETE FROM %s AS "t" WHERE %s RETURNING %s`, pgrTable(m.table), m.where, m.returning())
	}
}

func (m *pgrMutation) insertSQL() string {
	jt := m.jsonType()
	var source string
	columns := ""
	if len(m.written) == 0 {
		source = fmt.Sprintf("%s_array_elements(%s) _", jt, m.rowsExpression())
	} else {
		source = fmt.Sprintf("%s_to_recordset(%s) AS _(%s)", jt, m.rowsExpression(), m.typed())
		columns = " (" + strings.Join(m.quotedWritten(), ", ") + ")"
	}
	selection := make([]string, 0, len(m.written))
	for _, column := range m.written {
		selection = append(selection, `"pgrst_body".`+pgrQuote(column.name))
	}
	lateral := fmt.Sprintf("LATERAL (SELECT %s FROM %s) pgrst_body", strings.Join(m.quotedWritten(), ", "), source)
	if len(m.written) == 0 {
		lateral = fmt.Sprintf("LATERAL (SELECT FROM %s) pgrst_body", source)
	}
	var onConflict string
	if m.upsert && len(m.conflict) > 0 {
		target := make([]string, 0, len(m.conflict))
		for _, name := range m.conflict {
			target = append(target, pgrQuote(name))
		}
		if m.merge && len(m.written) > 0 {
			sets := make([]string, 0, len(m.written))
			for _, column := range m.written {
				sets = append(sets, pgrQuote(column.name)+" = EXCLUDED."+pgrQuote(column.name))
			}
			onConflict = fmt.Sprintf(" ON CONFLICT (%s) DO UPDATE SET %s", strings.Join(target, ", "), strings.Join(sets, ", "))
		} else {
			onConflict = fmt.Sprintf(" ON CONFLICT (%s) DO NOTHING", strings.Join(target, ", "))
		}
	}
	return fmt.Sprintf(`INSERT INTO %s AS "t"%s SELECT %s FROM (SELECT $%d::%s AS json_data) pgrst_payload, %s%s RETURNING %s`,
		pgrTable(m.table), columns, strings.Join(selection, ", "), m.bodyArg, m.jsonType(), lateral, onConflict, m.returning())
}

func (m *pgrMutation) updateSQL() string {
	sets := make([]string, 0, len(m.written))
	for _, column := range m.written {
		sets = append(sets, pgrQuote(column.name)+` = "pgrst_body".`+pgrQuote(column.name))
	}
	return fmt.Sprintf(`UPDATE %s AS "t" SET %s FROM (SELECT $%d::json AS json_data) pgrst_payload, LATERAL (SELECT %s FROM json_to_record(pgrst_payload.json_data) AS _(%s)) pgrst_body WHERE %s RETURNING %s`,
		pgrTable(m.table), strings.Join(sets, ", "), m.bodyArg, strings.Join(m.quotedWritten(), ", "), m.typed(), m.where, m.returning())
}

// pgrMutationResult is what one write reports: how many rows it changed, how
// many of them were new, and the rows when they come back.
type pgrMutationResult struct {
	total    int64
	inserted int64
	rows     []json.RawMessage
	// key are the primary key values, as text and in the order of the
	// mutation's locate, of the one row a return=headers-only insert wrote.
	key []*string
	// text is the body of a representation that is not a JSON array: CSV or
	// GeoJSON.
	text string
}

// run executes the write and reads its result. The rows are aggregated in the
// Database only while the count is within the quota, so a write that is about
// to be refused does not build a body it will not send.
func (m *pgrMutation) run(ctx context.Context, tx *sql.Tx, args []any) (pgrMutationResult, error) {
	var result pgrMutationResult
	inserted := "0::bigint"
	if m.tracksInserted {
		inserted = `(SELECT count(*) FROM "pgrst_w" WHERE "pgrst_inserted")`
	}
	rows := "'[]'::text"
	if m.representation {
		columns := make([]string, 0, len(m.selected))
		for _, name := range m.selected {
			if name == "*" {
				for _, column := range m.schema {
					columns = append(columns, `"t".`+pgrQuote(column.name))
				}
				continue
			}
			columns = append(columns, `"t".`+pgrQuote(name))
		}
		page := fmt.Sprintf(`SELECT %s FROM "pgrst_w" AS "t"%s`, strings.Join(columns, ", "), m.order)
		rows = fmt.Sprintf(`CASE WHEN (SELECT count(*) FROM "pgrst_w") > %d THEN '[]' ELSE (%s) END`,
			m.quota, m.media.aggregate(page, "x", false))
	}
	location := "NULL::text"
	if m.headersOnly && m.kind == pgrInsert && len(m.locate) > 0 {
		// Exactly one row written, or no Location (MainTx.hs, locF).
		keys := make([]string, 0, len(m.locate))
		for i := range m.locate {
			keys = append(keys, fmt.Sprintf(`"t"."pgrst_loc_%d"::text`, i))
		}
		location = fmt.Sprintf(`(SELECT json_build_array(%s)::text FROM "pgrst_w" AS "t" WHERE (SELECT count(*) FROM "pgrst_w") = 1)`,
			strings.Join(keys, ", "))
	}
	query := fmt.Sprintf(`WITH "pgrst_w" AS (%s) SELECT (SELECT count(*) FROM "pgrst_w"), %s, %s, %s`, m.statement(), inserted, rows, location)
	var out string
	var key sql.NullString
	if err := tx.QueryRowContext(ctx, query, args...).Scan(&result.total, &result.inserted, &out, &key); err != nil {
		return result, err
	}
	if key.Valid {
		if err := json.Unmarshal([]byte(key.String), &result.key); err != nil {
			return result, err
		}
	}
	if m.representation {
		if !m.media.isJSON() {
			result.text = out
			return result, nil
		}
		if err := json.Unmarshal([]byte(out), &result.rows); err != nil {
			return result, err
		}
	}
	return result, nil
}

// pgrInTableOrder is the names of key in the order the table's columns come,
// which is the order PostgREST's Location lists them in (SqlFragment.hs,
// locationF reads them from the row).
func pgrInTableOrder(key []string, columns []pgrTableColumn) []string {
	var ordered []string
	for _, column := range columns {
		for _, name := range key {
			if name == column.name {
				ordered = append(ordered, name)
			}
		}
	}
	return ordered
}

// location is the Location header of an insert that wrote one row: the table,
// then each primary key column as column=eq.value, or is.null for a NULL, the
// way renderSimpleQuery True writes a query (Response.hs). Empty when the row
// is not one, or the table has no primary key.
func (r pgrMutationResult) location(table string, locate []string) string {
	if len(r.key) == 0 || len(r.key) != len(locate) {
		return ""
	}
	escape := func(text string) string { return strings.ReplaceAll(url.QueryEscape(text), "+", "%20") }
	pairs := make([]string, 0, len(locate))
	for i, name := range locate {
		value := "is.null"
		if r.key[i] != nil {
			value = "eq." + *r.key[i]
		}
		pairs = append(pairs, escape(name)+"="+escape(value))
	}
	return "/" + table + "?" + strings.Join(pairs, "&")
}
