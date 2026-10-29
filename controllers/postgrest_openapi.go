package controllers

// miniship (miniship-cloud#1890, ADR 0075): GET and HEAD on /rest/v1/, the
// address supabase-js's getOpenApiSpec() reads. PostgREST 14 answers it with
// the Swagger 2.0 document of the schema (Response/OpenAPI.hs, v14.18), and a
// Supabase hosted Project answers it to a secret key only: the changelog of
// 17 February 2026, "Removing access to OpenAPI spec via the anon key", says
// the publishable (anon) key is refused with a fixed message and a hint, and
// that the secret and service_role keys keep the document. OPENAPI-FINDINGS
// in the pull request has the sources.
//
// So the document goes to service_role, and every other caller, a signed-in
// User's token included, is refused with Supabase's body. The document is the
// one PostgREST writes in its default openapi-mode, follow-privileges: it
// lists only what the caller's role may touch, read from the catalog inside
// the caller's own transaction, so the role's grants decide what is there.
//
// The queries below are SchemaCache.hs's tablesSqlQuery and
// SqlFragment.hs's accessibleTables and baseFuncSqlQuery, narrowed to the one
// schema rest serves; the document is OpenAPI.hs, field by field.

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"sort"
	"strings"

	"github.com/lib/pq"
	"github.com/structy/log"

	"github.com/prest/prest/v2/adapters"
	"github.com/prest/prest/v2/internal/logsafe"
)

const (
	// openAPIRole is the role of the Project's secret key: the one caller the
	// document is for.
	openAPIRole = "service_role"
	// openAPIMediaType is toContentType MTOpenAPI (MediaType.hs).
	openAPIMediaType = "application/openapi+json; charset=utf-8"
	// The refusal Supabase writes to every other caller.
	openAPIRefusedMessage = "Access to schema is forbidden"
	openAPIRefusedHint    = "Accessing the schema via the Data API is only allowed using a secret API key."
	// What PostgREST 14.18 writes for itself: prettyVersion, docsVersion, and
	// the proxy defaults of an unset openapi-server-proxy-uri.
	openAPIVersion     = "14.18"
	openAPIDocsVersion = "v14"
	openAPIHost        = "0.0.0.0:3000"
)

// PostgRESTOpenAPI serves GET and HEAD on /rest/v1/.
func (h *CRUDHandler) PostgRESTOpenAPI(w http.ResponseWriter, r *http.Request) {
	if profile := r.Header.Get("Accept-Profile"); profile != "" && profile != pgrSchema {
		pgrWriteError(w, pgrBadSchema(profile))
		return
	}
	project, adapter, err := h.resolveProject(r)
	if err != nil {
		pgrWriteError(w, err)
		return
	}
	caller, err := h.resolveCaller(r, project, adapter)
	if err != nil {
		pgrWriteError(w, err)
		return
	}
	if caller.Role != openAPIRole {
		writeOpenAPIRefusal(w)
		return
	}
	if err := pgrOpenAPIAccepted(r.Header.Get("Accept")); err != nil {
		pgrWriteError(w, err)
		return
	}

	reader, ok := adapter.(adapters.CallerReader)
	if !ok {
		pgrWriteError(w, &pgrError{status: http.StatusInternalServerError, code: "PGRST000", message: adapters.ErrRoleNotEntered.Error()})
		return
	}
	ctx, cancel := requestContext(r, project)
	defer cancel()

	var body []byte
	build := func(reader adapters.CallerReader) error {
		return reader.ReadAsCallerCtx(ctx, caller, func(tx *sql.Tx) error {
			doc, err := readOpenAPI(ctx, tx)
			if err != nil {
				return err
			}
			body, err = encodeOpenAPI(doc)
			return err
		})
	}
	err = build(reader)
	if err != nil && credentialWasRotated(err) && h.admitter != nil {
		if fresh, freshErr := h.admitter.Readmit(ctx, project); freshErr == nil {
			if freshReader, ok := fresh.(adapters.CallerReader); ok {
				err = build(freshReader)
			}
		}
	}
	if err != nil {
		log.Errorln(logsafe.Error(err))
		pgrWriteError(w, pgrReadFailure(err, true))
		return
	}
	w.Header().Set("Content-Type", openAPIMediaType)
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = w.Write(body)
	}
}

// writeOpenAPIRefusal is Supabase's answer to a caller that is not holding a
// secret key: 403 and two fields, with no code (changelog 42949).
func writeOpenAPIRefusal(w http.ResponseWriter) {
	body, _ := json.Marshal(map[string]string{"message": openAPIRefusedMessage, "hint": openAPIRefusedHint})
	w.Header().Set("Content-Type", pgrJSONMediaType)
	w.WriteHeader(http.StatusForbidden)
	_, _ = w.Write(body)
}

// pgrOpenAPIAccepted is inspectPlan (Plan.hs): the root is answered when the
// Accept names openapi+json, application/json or anything, and is 406
// PGRST107 otherwise, listing every entry as PostgREST writes it.
func pgrOpenAPIAccepted(accept string) error {
	if accept == "" {
		return nil
	}
	entries := pgrParseAccept(accept)
	listed := make([]string, 0, len(entries))
	for _, entry := range entries {
		switch entry.mime {
		case "application/openapi+json", "application/json", "*/*":
			return nil
		}
		listed = append(listed, entry.mime)
	}
	return &pgrError{
		status:  http.StatusNotAcceptable,
		code:    "PGRST107",
		message: "None of these media types are available: " + strings.Join(listed, ", "),
	}
}

// ---- what the catalog says ----

type openAPIColumn struct {
	Name        string   `json:"name"`
	Description *string  `json:"description"`
	Nullable    bool     `json:"nullable"`
	Type        string   `json:"type"`
	MaxLength   *int     `json:"maxLength"`
	Default     *string  `json:"default"`
	Enum        []string `json:"enum"`
}

type openAPITable struct {
	Name        string
	Description *string
	Insertable  bool
	Updatable   bool
	Deletable   bool
	PKCols      []string
	Columns     []openAPIColumn
}

type openAPIParam struct {
	Name     string `json:"name"`
	Type     string `json:"type"`
	Required bool   `json:"required"`
	Variadic bool   `json:"variadic"`
}

type openAPIRoutine struct {
	Name        string
	Description *string
	Params      []openAPIParam
	Volatility  string
}

type openAPIFK struct{ table, column string }

type openAPISource struct {
	title    *string
	tables   []openAPITable
	fks      map[[2]string]openAPIFK // (table, column) -> the column it references
	routines []openAPIRoutine
}

// The privilege filter of accessibleTables (SqlFragment.hs): what the current
// role owns or holds any privilege on.
const openAPITablesSQL = `
WITH
base_types AS (
  WITH RECURSIVE
  recurse AS (
    SELECT oid, typbasetype, typnamespace AS base_namespace, COALESCE(NULLIF(typbasetype, 0), oid) AS base_type
    FROM pg_type
    UNION
    SELECT t.oid, b.typbasetype, b.typnamespace AS base_namespace, COALESCE(NULLIF(b.typbasetype, 0), b.oid) AS base_type
    FROM recurse t JOIN pg_type b ON t.typbasetype = b.oid
  )
  SELECT oid, base_namespace, base_type FROM recurse WHERE typbasetype = 0
),
columns AS (
  SELECT
    c.oid AS relid,
    a.attname::name AS column_name,
    d.description AS description,
    CASE
      WHEN (t.typbasetype != 0) AND (ad.adbin IS NULL) THEN pg_get_expr(t.typdefaultbin, 0)
      WHEN a.attidentity  = 'd' THEN format('nextval(%L)', seq.objid::regclass)
      WHEN a.attgenerated = 's' THEN null
      ELSE pg_get_expr(ad.adbin, ad.adrelid)::text
    END AS column_default,
    not (a.attnotnull OR t.typtype = 'd' AND t.typnotnull) AS is_nullable,
    CASE
      WHEN t.typtype = 'd' THEN
        CASE
          WHEN bt.base_namespace = 'pg_catalog'::regnamespace THEN format_type(bt.base_type, NULL::integer)
          ELSE format_type(a.atttypid, a.atttypmod)
        END
      ELSE
        CASE
          WHEN t.typnamespace = 'pg_catalog'::regnamespace THEN format_type(a.atttypid, NULL::integer)
          ELSE format_type(a.atttypid, a.atttypmod)
        END
    END::text AS data_type,
    information_schema._pg_char_max_length(
      information_schema._pg_truetypid(a.*, t.*),
      information_schema._pg_truetypmod(a.*, t.*)
    )::integer AS character_maximum_length,
    bt.base_type,
    a.attnum::integer AS position
  FROM pg_attribute a
    LEFT JOIN pg_description AS d ON d.objoid = a.attrelid and d.objsubid = a.attnum and d.classoid = 'pg_class'::regclass
    LEFT JOIN pg_attrdef ad ON a.attrelid = ad.adrelid AND a.attnum = ad.adnum
    JOIN pg_class c ON a.attrelid = c.oid
    JOIN pg_type t ON a.atttypid = t.oid
    LEFT JOIN base_types bt ON t.oid = bt.oid
    LEFT JOIN pg_depend seq ON seq.refobjid = a.attrelid and seq.refobjsubid = a.attnum and seq.deptype = 'i'
  WHERE NOT pg_is_other_temp_schema(c.relnamespace)
    AND a.attnum > 0
    AND NOT a.attisdropped
    AND c.relkind in ('r', 'v', 'f', 'm', 'p')
    AND c.relnamespace = $1::text::regnamespace
),
columns_agg AS (
  SELECT relid,
    json_agg(json_build_object(
      'name', column_name,
      'description', description,
      'nullable', is_nullable,
      'type', data_type,
      'maxLength', character_maximum_length,
      'default', column_default,
      'enum', coalesce((SELECT json_agg(enumlabel ORDER BY enumsortorder) FROM pg_enum WHERE enumtypid = base_type), '[]'::json)
    ) ORDER BY position) AS columns
  FROM columns
  GROUP BY relid
),
tbl_pk_cols AS (
  SELECT r.oid AS relid, array_agg(a.attname ORDER BY a.attname) AS pk_cols
  FROM pg_class r
  JOIN pg_constraint c ON r.oid = c.conrelid
  JOIN pg_attribute a ON a.attrelid = r.oid AND a.attnum = ANY (c.conkey)
  WHERE c.contype in ('p')
    AND r.relkind IN ('r', 'p')
    AND r.relnamespace = $1::text::regnamespace
    AND NOT a.attisdropped
  GROUP BY r.oid
)
SELECT
  c.relname::text,
  d.description,
  (c.relkind IN ('r','p') OR (c.relkind in ('v','f') AND (pg_relation_is_updatable(c.oid::regclass, TRUE) & 8) = 8)),
  (c.relkind IN ('r','p') OR (c.relkind in ('v','f') AND (pg_relation_is_updatable(c.oid::regclass, TRUE) & 4) = 4)),
  (c.relkind IN ('r','p') OR (c.relkind in ('v','f') AND (pg_relation_is_updatable(c.oid::regclass, TRUE) & 16) = 16)),
  to_json(coalesce(tpks.pk_cols, '{}'::name[])),
  coalesce(cols_agg.columns, '[]'::json)
FROM pg_class c
LEFT JOIN pg_description d ON d.objoid = c.oid AND d.objsubid = 0 AND d.classoid = 'pg_class'::regclass
LEFT JOIN tbl_pk_cols tpks ON c.oid = tpks.relid
LEFT JOIN columns_agg cols_agg ON c.oid = cols_agg.relid
WHERE c.relkind IN ('v','r','m','f','p')
  AND c.relnamespace = $1::text::regnamespace
  AND not c.relispartition
  AND (
    pg_has_role(c.relowner, 'USAGE')
    or has_table_privilege(c.oid, 'SELECT, INSERT, UPDATE, DELETE, TRUNCATE, REFERENCES, TRIGGER')
    or has_any_column_privilege(c.oid, 'SELECT, INSERT, UPDATE, REFERENCES')
  )
ORDER BY c.relname`

// Every single-column foreign key that ends in the schema: the only ones a
// column's note can name (OpenAPI.hs, makeProperty). PostgREST drops a
// relationship whose foreign table is outside the exposed schemas; the table
// it starts from may be anywhere, because a view of it is what is described.
const openAPIForeignKeysSQL = `
SELECT ns.nspname::text, tab.relname::text, traint.conname::text, cols.attname::text, other.relname::text, refs.attname::text
FROM pg_constraint traint
JOIN pg_class tab ON tab.oid = traint.conrelid
JOIN pg_namespace ns ON ns.oid = tab.relnamespace
JOIN pg_class other ON other.oid = traint.confrelid
JOIN pg_attribute cols ON cols.attrelid = traint.conrelid AND cols.attnum = traint.conkey[1]
JOIN pg_attribute refs ON refs.attrelid = traint.confrelid AND refs.attnum = traint.confkey[1]
WHERE traint.contype = 'f'
  AND traint.conparentid = 0
  AND array_length(traint.conkey, 1) = 1
  AND other.relnamespace = $1::text::regnamespace
ORDER BY traint.conrelid, traint.conname`

// baseFuncSqlQuery (SqlFragment.hs) with the arguments as JSON.
const openAPIRoutinesSQL = `
WITH
base_types AS (
  WITH RECURSIVE
  recurse AS (
    SELECT oid, typbasetype, typnamespace AS base_namespace, COALESCE(NULLIF(typbasetype, 0), oid) AS base_type
    FROM pg_type
    UNION
    SELECT t.oid, b.typbasetype, b.typnamespace AS base_namespace, COALESCE(NULLIF(b.typbasetype, 0), b.oid) AS base_type
    FROM recurse t JOIN pg_type b ON t.typbasetype = b.oid
  )
  SELECT oid, base_namespace, base_type FROM recurse WHERE typbasetype = 0
),
arguments AS (
  SELECT
    oid,
    json_agg(json_build_object(
      'name', COALESCE(name, ''),
      'type', type::regtype::text,
      'required', idx <= (pronargs - pronargdefaults),
      'variadic', COALESCE(mode = 'v', FALSE)
    ) ORDER BY idx) AS args,
    CASE COUNT(*) - COUNT(name)
      WHEN 0 THEN true
      WHEN 1 THEN (array_agg(type))[1] IN ('bytea'::regtype, 'json'::regtype, 'jsonb'::regtype, 'text'::regtype, 'xml'::regtype)
      ELSE false
    END AS callable
  FROM pg_proc,
       unnest(proargnames, proargtypes, proargmodes) WITH ORDINALITY AS _ (name, type, mode, idx)
  WHERE type IS NOT NULL
  GROUP BY oid
)
SELECT
  p.proname::text,
  d.description,
  COALESCE(a.args, '[]'::json),
  p.provolatile::text
FROM pg_proc p
LEFT JOIN arguments a ON a.oid = p.oid
JOIN base_types bt ON bt.oid = p.prorettype
JOIN pg_type t ON t.oid = bt.base_type
LEFT JOIN pg_description AS d ON d.objoid = p.oid AND d.classoid = 'pg_proc'::regclass
WHERE t.oid <> 'trigger'::regtype AND COALESCE(a.callable, true)
  AND has_function_privilege(p.oid, 'execute')
  AND prokind = 'f'
  AND p.pronamespace = $1::text::regnamespace
ORDER BY p.proname, p.oid`

// readOpenAPI reads what the role may see and builds the document from it.
func readOpenAPI(ctx context.Context, tx *sql.Tx) (oaObj, error) {
	src := openAPISource{fks: map[[2]string]openAPIFK{}}

	// PostgREST reads its schema cache with the search path voided, so a type
	// outside pg_catalog is named with its schema (SchemaCache.hs, "set local
	// schema ''").
	if _, err := tx.ExecContext(ctx, `SET LOCAL search_path TO ''`); err != nil {
		return nil, err
	}
	if err := tx.QueryRowContext(ctx, `SELECT pg_catalog.obj_description($1::text::regnamespace, 'pg_namespace')`, pgrSchema).Scan(&src.title); err != nil {
		return nil, err
	}

	rows, err := tx.QueryContext(ctx, openAPITablesSQL, pgrSchema)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var t openAPITable
		var pk, cols []byte
		if err := rows.Scan(&t.Name, &t.Description, &t.Insertable, &t.Updatable, &t.Deletable, &pk, &cols); err != nil {
			_ = rows.Close()
			return nil, err
		}
		if err := json.Unmarshal(pk, &t.PKCols); err != nil {
			_ = rows.Close()
			return nil, err
		}
		if err := json.Unmarshal(cols, &t.Columns); err != nil {
			_ = rows.Close()
			return nil, err
		}
		src.tables = append(src.tables, t)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, err
	}
	_ = rows.Close()

	// The keys of every table that has one into the schema, by constraint,
	// for the notes of the table's own columns and of the views over it.
	type keyOf struct{ schema, table, constraint string }
	type column struct{ column, fTable, fColumn string }
	constraints := map[keyOf]column{}
	rows, err = tx.QueryContext(ctx, openAPIForeignKeysSQL, pgrSchema)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var schema, table, constraint string
		var c column
		if err := rows.Scan(&schema, &table, &constraint, &c.column, &c.fTable, &c.fColumn); err != nil {
			_ = rows.Close()
			return nil, err
		}
		constraints[keyOf{schema, table, constraint}] = c
		if schema == pgrSchema {
			// PostgREST collects a table's relationships by prepending, so with
			// two foreign keys on one column the later constraint is found first.
			src.fks[[2]string{table, c.column}] = openAPIFK{c.fTable, c.fColumn}
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, err
	}
	_ = rows.Close()

	// What a view carries of those keys, and of its tables' primary keys
	// (addViewM2OAndO2ORels, addViewPrimaryKeys).
	viewPK := map[string][]string{}
	rows, err = tx.QueryContext(ctx, openAPIViewKeysSQL, pq.Array([]string{pgrSchema}), pq.Array([]string{}))
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var tableSchema, tableName, viewSchema, viewName, constraint, kind string
		var raw []byte
		if err := rows.Scan(&tableSchema, &tableName, &viewSchema, &viewName, &constraint, &kind, &raw); err != nil {
			_ = rows.Close()
			return nil, err
		}
		var deps []struct {
			Col   string   `json:"col"`
			Views []string `json:"views"`
		}
		if err := json.Unmarshal(raw, &deps); err != nil {
			_ = rows.Close()
			return nil, err
		}
		if viewSchema != pgrSchema {
			continue
		}
		switch kind {
		case "p":
			// One reference to each key column is enough: the first.
			for _, dep := range deps {
				if len(dep.Views) > 0 {
					viewPK[viewName] = append(viewPK[viewName], dep.Views[0])
				}
			}
		case "f":
			key, ok := constraints[keyOf{tableSchema, tableName, constraint}]
			if !ok || len(deps) != 1 {
				continue
			}
			for _, viewColumn := range deps[0].Views {
				if _, taken := src.fks[[2]string{viewName, viewColumn}]; !taken {
					src.fks[[2]string{viewName, viewColumn}] = openAPIFK{key.fTable, key.fColumn}
				}
			}
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, err
	}
	_ = rows.Close()
	for i := range src.tables {
		if pks, ok := viewPK[src.tables[i].Name]; ok {
			src.tables[i].PKCols = pks
		}
	}

	rows, err = tx.QueryContext(ctx, openAPIRoutinesSQL, pgrSchema)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var f openAPIRoutine
		var args []byte
		if err := rows.Scan(&f.Name, &f.Description, &args, &f.Volatility); err != nil {
			_ = rows.Close()
			return nil, err
		}
		if err := json.Unmarshal(args, &f.Params); err != nil {
			_ = rows.Close()
			return nil, err
		}
		src.routines = append(src.routines, f)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, err
	}
	_ = rows.Close()

	return buildOpenAPI(src), nil
}

// ---- the document (OpenAPI.hs) ----

// encodeOpenAPI is the document as aeson writes it: no HTML escaping, no
// trailing newline.
func encodeOpenAPI(doc oaObj) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(doc); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

type oaObj = map[string]any

func oaRef(kind, name string) oaObj { return oaObj{"$ref": "#/" + kind + "/" + name} }

// firstLine splits a comment as PostgREST does: the summary is up to the first
// newline, the description is the rest without its leading newlines.
func firstLine(text *string) (summary, description *string) {
	if text == nil {
		return nil, nil
	}
	head, rest, found := strings.Cut(*text, "\n")
	summary = &head
	if found {
		rest = strings.TrimLeft(rest, "\n")
		if rest != "" {
			description = &rest
		}
	}
	return summary, description
}

func buildOpenAPI(src openAPISource) oaObj {
	title, description := "PostgREST API", "This is a dynamic API generated by PostgREST"
	if t, d := firstLine(src.title); t != nil {
		title = *t
		// A schema comment of one line leaves the description empty, as
		// PostgREST writes it (fromMaybe over Just "").
		description = ""
		if d != nil {
			description = *d
		}
	}

	definitions, parameters, paths := oaObj{}, oaObj{}, oaObj{}
	for name, param := range staticOpenAPIParameters() {
		parameters[name] = param
	}
	paths["/"] = oaObj{"get": oaObj{
		"tags":      []string{"Introspection"},
		"summary":   "OpenAPI description (this document)",
		"produces":  []string{"application/openapi+json", "application/json"},
		"responses": oaObj{"200": oaObj{"description": "OK"}},
	}}

	for _, t := range src.tables {
		definitions[t.Name] = tableDefinition(t, src.fks)
		parameters["body."+t.Name] = oaObj{
			"name": t.Name, "description": t.Name, "required": false, "in": "body",
			"schema": oaRef("definitions", t.Name),
		}
		for _, c := range t.Columns {
			param := oaObj{"name": c.Name, "required": false, "in": "query", "type": "string"}
			if c.Description != nil {
				param["description"] = *c.Description
			}
			parameters["rowFilter."+t.Name+"."+c.Name] = param
		}
		paths["/"+t.Name] = tablePath(t)
	}
	// With overloads of one name PostgREST keeps the last path written, and
	// writes them in the order of its Ord for a routine.
	sort.SliceStable(src.routines, func(i, j int) bool { return routineLess(src.routines[i], src.routines[j]) })
	for _, f := range src.routines {
		paths["/rpc/"+f.Name] = routinePath(f)
	}

	mimes := []string{"application/json", "application/vnd.pgrst.object+json;nulls=stripped", "application/vnd.pgrst.object+json", "text/csv"}
	return oaObj{
		"swagger":  "2.0",
		"basePath": "/",
		"schemes":  []string{"http"},
		"info":     oaObj{"version": openAPIVersion, "title": title, "description": description},
		"externalDocs": oaObj{
			"description": "PostgREST Documentation",
			"url":         "https://postgrest.org/en/" + openAPIDocsVersion + "/references/api.html",
		},
		"host":        openAPIHost,
		"definitions": definitions,
		"parameters":  parameters,
		"paths":       paths,
		"produces":    mimes,
		"consumes":    mimes,
	}
}

func tableDefinition(t openAPITable, fks map[[2]string]openAPIFK) oaObj {
	def := oaObj{"type": "object"}
	if t.Description != nil {
		def["description"] = *t.Description
	}
	properties := oaObj{}
	required := []string{}
	pk := map[string]bool{}
	for _, name := range t.PKCols {
		pk[name] = true
	}
	for _, c := range t.Columns {
		properties[c.Name] = columnProperty(t.Name, c, pk[c.Name], fks)
		if !c.Nullable {
			required = append(required, c.Name)
		}
	}
	def["properties"] = properties
	if len(required) > 0 {
		def["required"] = required
	}
	return def
}

func columnProperty(table string, c openAPIColumn, isPK bool, fks map[[2]string]openAPIFK) oaObj {
	s := oaObj{}
	if c.Default != nil {
		if v, ok := openAPIDefault(c.Type, *c.Default); ok {
			s["default"] = v
		}
	}
	notes := []string{"Note:"}
	if isPK {
		notes = append(notes, "This is a Primary Key.<pk/>")
	}
	if fk, ok := fks[[2]string{table, c.Name}]; ok {
		notes = append(notes, "This is a Foreign Key to `"+fk.table+"."+fk.column+"`.<fk table='"+fk.table+"' column='"+fk.column+"'/>")
	}
	switch {
	case len(notes) > 1:
		d := ""
		if c.Description != nil {
			d = *c.Description + "\n\n"
		}
		s["description"] = d + strings.Join(notes, "\n")
	case c.Description != nil:
		s["description"] = *c.Description
	}
	if len(c.Enum) > 0 {
		s["enum"] = c.Enum
	}
	s["format"] = swaggerFormat(c.Type)
	if c.MaxLength != nil {
		s["maxLength"] = *c.MaxLength
	}
	if typ, ok := swaggerType(c.Type); ok {
		s["type"] = typ
	}
	if items := propertyItems(c.Type); items != nil {
		s["items"] = items
	}
	return s
}

// swaggerType is toSwaggerType: false for json and jsonb, which have none.
func swaggerType(colType string) (string, bool) {
	switch colType {
	case "character varying", "character", "text":
		return "string", true
	case "boolean":
		return "boolean", true
	case "smallint", "integer", "bigint":
		return "integer", true
	case "numeric", "real", "double precision":
		return "number", true
	case "json", "jsonb":
		return "", false
	}
	if strings.HasSuffix(colType, "[]") {
		return "array", true
	}
	return "string", true
}

func swaggerFormat(colType string) string {
	switch colType {
	case "smallint", "integer":
		return "int32"
	case "bigint":
		return "int64"
	}
	return colType
}

// propertyItems is makePropertyItems: the element type of an array column.
func propertyItems(colType string) oaObj {
	if typ, _ := swaggerType(colType); typ != "array" {
		return nil
	}
	items := oaObj{}
	if typ, ok := swaggerType(strings.TrimSuffix(colType, "[]")); ok {
		items["type"] = typ
	}
	return items
}

// openAPIDefault is parseDefault then aeson's decode: a default that is not
// JSON once PostgREST has unquoted it is left out.
func openAPIDefault(colType, def string) (any, bool) {
	if typ, ok := swaggerType(colType); ok && typ == "string" {
		unquoted := def
		if stripped, found := strings.CutSuffix(def, "::"+colType); found {
			unquoted = strings.Trim(stripped, "'")
		}
		def = `"` + unquoted + `"`
	}
	var v any
	if err := json.Unmarshal([]byte(def), &v); err != nil {
		return nil, false
	}
	return v, true
}

// routineLess is the derived Ord of PostgREST's Routine among overloads of one
// name: the description (none first), then the parameters one by one by name,
// type, required and variadic, a shorter list first.
func routineLess(a, b openAPIRoutine) bool {
	if a.Name != b.Name {
		return a.Name < b.Name
	}
	switch {
	case a.Description == nil && b.Description != nil:
		return true
	case a.Description != nil && b.Description == nil:
		return false
	case a.Description != nil && *a.Description != *b.Description:
		return *a.Description < *b.Description
	}
	for i := 0; i < len(a.Params) && i < len(b.Params); i++ {
		x, y := a.Params[i], b.Params[i]
		switch {
		case x.Name != y.Name:
			return x.Name < y.Name
		case x.Type != y.Type:
			return x.Type < y.Type
		case x.Required != y.Required:
			return !x.Required
		case x.Variadic != y.Variadic:
			return !x.Variadic
		}
	}
	return len(a.Params) < len(b.Params)
}

func tablePath(t openAPITable) oaObj {
	summary, description := firstLine(t.Description)
	op := func(params []string, extra oaObj) oaObj {
		o := oaObj{"tags": []string{t.Name}}
		if summary != nil {
			o["summary"] = *summary
		}
		if description != nil {
			o["description"] = *description
		}
		refs := make([]oaObj, 0, len(params))
		for _, p := range params {
			refs = append(refs, oaRef("parameters", p))
		}
		o["parameters"] = refs
		o["responses"] = extra
		return o
	}
	rows := make([]string, 0, len(t.Columns))
	for _, c := range t.Columns {
		rows = append(rows, "rowFilter."+t.Name+"."+c.Name)
	}
	body := "body." + t.Name
	item := oaObj{"get": op(append(append([]string{}, rows...), "select", "order", "range", "rangeUnit", "offset", "limit", "preferCount"), oaObj{
		"206": oaObj{"description": "Partial Content"},
		"200": oaObj{"description": "OK", "schema": oaObj{"type": "array", "items": oaRef("definitions", t.Name)}},
	})}
	if t.Insertable || t.Updatable || t.Deletable {
		item["post"] = op([]string{body, "select", "preferPost"}, oaObj{"201": oaObj{"description": "Created"}})
		item["patch"] = op(append(append([]string{}, rows...), body, "preferReturn"), oaObj{"204": oaObj{"description": "No Content"}})
		item["delete"] = op(append(append([]string{}, rows...), "preferReturn"), oaObj{"204": oaObj{"description": "No Content"}})
	}
	return item
}

func routinePath(f openAPIRoutine) oaObj {
	summary, description := firstLine(f.Description)
	op := func(params any) oaObj {
		o := oaObj{
			"tags":       []string{"(rpc) " + f.Name},
			"produces":   []string{"application/json", "application/vnd.pgrst.object+json;nulls=stripped", "application/vnd.pgrst.object+json"},
			"responses":  oaObj{"200": oaObj{"description": "OK"}},
			"parameters": params,
		}
		if summary != nil {
			o["summary"] = *summary
		}
		if description != nil {
			o["description"] = *description
		}
		return o
	}

	properties, required := oaObj{}, []string{}
	getParams := make([]oaObj, 0, len(f.Params))
	for _, p := range f.Params {
		prop := oaObj{"format": swaggerFormat(p.Type)}
		if typ, ok := swaggerType(p.Type); ok {
			prop["type"] = typ
		}
		if items := propertyItems(p.Type); items != nil {
			prop["items"] = items
		}
		properties[p.Name] = prop
		if p.Required {
			required = append(required, p.Name)
		}
		getParams = append(getParams, routineGetParam(p))
	}
	schema := oaObj{"type": "object"}
	if len(properties) > 0 {
		schema["properties"] = properties
	}
	if f.Description != nil {
		schema["description"] = *f.Description
	}
	if len(required) > 0 {
		schema["required"] = required
	}
	post := op([]oaObj{
		{"name": "args", "required": true, "in": "body", "schema": schema},
		oaRef("parameters", "preferParams"),
	})
	if f.Volatility == "v" {
		return oaObj{"post": post}
	}
	get := op(getParams)
	if len(getParams) == 0 {
		delete(get, "parameters")
	}
	return oaObj{"get": get, "post": post}
}

// routineGetParam is makeProcGetParam.
func routineGetParam(p openAPIParam) oaObj {
	param := oaObj{"name": p.Name, "required": p.Required, "in": "query"}
	if p.Variadic {
		typ, ok := swaggerType(p.Type)
		if !ok {
			typ = "string"
		}
		element := strings.TrimSuffix(p.Type, "[]")
		items := oaObj{"collectionFormat": "multi", "format": swaggerFormat(element)}
		if et, ok := swaggerType(element); ok {
			items["type"] = et
		}
		param["type"], param["items"] = typ, items
		return param
	}
	typ, ok := swaggerType(p.Type)
	if !ok || typ == "array" {
		typ = "string"
	}
	param["type"], param["format"] = typ, swaggerFormat(p.Type)
	return param
}

func staticOpenAPIParameters() oaObj {
	prefer := func(values ...string) oaObj {
		p := oaObj{"name": "Prefer", "description": "Preference", "required": false, "in": "header", "type": "string"}
		if len(values) > 0 {
			p["enum"] = values
		}
		return p
	}
	query := func(name, description string) oaObj {
		return oaObj{"name": name, "description": description, "required": false, "in": "query", "type": "string"}
	}
	return oaObj{
		"preferParams": prefer(),
		"preferReturn": prefer("return=representation", "return=minimal", "return=none"),
		"preferCount":  prefer("count=none"),
		"preferPost":   prefer("return=representation", "return=minimal", "return=none", "resolution=ignore-duplicates", "resolution=merge-duplicates"),
		"select":       query("select", "Filtering Columns"),
		"on_conflict":  query("on_conflict", "On Conflict"),
		"order":        query("order", "Ordering"),
		"range":        oaObj{"name": "Range", "description": "Limiting and Pagination", "required": false, "in": "header", "type": "string"},
		"rangeUnit":    oaObj{"name": "Range-Unit", "description": "Limiting and Pagination", "required": false, "in": "header", "type": "string", "default": "items"},
		"offset":       query("offset", "Limiting and Pagination"),
		"limit":        query("limit", "Limiting and Pagination"),
	}
}
