package controllers

// miniship (miniship-cloud#1888, ADR 0075): a table the Database does not have
// is PostgREST 14's PGRST205, 404, found before the request is planned (Plan.hs,
// findTable) with the hint its schema cache computes (Error.hs, tableNotFoundHint).
//
// PostgREST's schema cache lists every table, view, materialised view, foreign
// table and partitioned table of the schema, whether or not the caller may read
// it (SchemaCache.hs, allTables); one the caller may not read is the Database's
// own refusal when the query runs, not PGRST205. rest reads the same list from
// the catalog, which any role may read, and only when it has a reason to ask: a
// statement that failed with 42P01, or a refusal PostgREST raises after the
// table is found.

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"

	"github.com/prest/prest/v2/adapters"
	"github.com/prest/prest/v2/internal/pgrwire"
)

// pgrRelationsFilter is the kinds of relation the schema cache holds: v, r, m, f, p.
const pgrRelationsFilter = `c.relnamespace = (SELECT oid FROM pg_catalog.pg_namespace WHERE nspname = $1)
		AND c.relkind IN ('v','r','m','f','p') AND NOT c.relispartition`

// pgrMissingTable is nil when table is in the schema, and PGRST205 when it is
// not. A catalog that cannot be read is nil too: the caller's own error stands.
func pgrMissingTable(ctx context.Context, tx *sql.Tx, table string) *pgrError {
	var exists bool
	if err := tx.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM pg_catalog.pg_class c WHERE `+pgrRelationsFilter+` AND c.relname = $2)`,
		pgrSchema, table).Scan(&exists); err != nil || exists {
		return nil
	}
	hint := ""
	// PostgREST has no fuzzy index for a schema of pgrwire.MaxFuzzyTables tables
	// or more, so none is read.
	rows, err := tx.QueryContext(ctx,
		`SELECT c.relname::text FROM pg_catalog.pg_class c WHERE `+pgrRelationsFilter+` ORDER BY c.relname LIMIT $2`,
		pgrSchema, pgrwire.MaxFuzzyTables)
	if err == nil {
		var names []string
		for rows.Next() {
			var name string
			if rows.Scan(&name) == nil {
				names = append(names, name)
			}
		}
		_ = rows.Close()
		hint = pgrwire.SuggestTable(names, table)
	}
	return pgrTableNotFound(table, hint)
}

// pgrTableNotFound is PGRST205 (Error.hs, TableNotFound), and hint the closest
// table's name or "".
func pgrTableNotFound(table, hint string) *pgrError {
	missing := &pgrError{
		status:  http.StatusNotFound,
		code:    "PGRST205",
		message: fmt.Sprintf("Could not find the table '%s.%s' in the schema cache", pgrSchema, table),
	}
	if hint != "" {
		missing.hint = fmt.Sprintf("Perhaps you meant the table '%s.%s'", pgrSchema, hint)
	}
	return missing
}

// tableNotFound asks the Database, as the caller, whether table is missing
// after a statement failed with 42P01.
func (h *CRUDHandler) tableNotFound(ctx context.Context, adapter adapters.Adapter, caller adapters.Caller, table string) *pgrError {
	reader, ok := adapter.(adapters.CallerReader)
	if !ok {
		return nil
	}
	var missing *pgrError
	_ = reader.ReadAsCallerCtx(ctx, caller, func(tx *sql.Tx) error {
		missing = pgrMissingTable(ctx, tx, table)
		return nil
	})
	return missing
}
