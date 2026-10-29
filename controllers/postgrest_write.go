package controllers

// miniship (miniship-cloud#1303, public ADR 0077): POST, PATCH and DELETE on
// /rest/v1/{table}, the address the Supabase SDK's database client writes. They
// follow PostgREST v12, the version the SDK at 2.117.3 expects:
//
//   - POST inserts a JSON object or an array of objects, and 201 answers it.
//     Prefer: resolution=merge-duplicates makes it an upsert that updates the
//     row a conflict names (200 when no row was new), and resolution=
//     ignore-duplicates one that leaves it. on_conflict names the columns of
//     the conflict, the primary key when it is absent. missing=default gives a
//     column its default where a row omits the key.
//   - PATCH sets the columns its JSON object names on the rows its filters
//     match. 200 with the rows, or 204.
//   - DELETE removes the rows its filters match. 200 with the rows, or 204.
//
// Prefer: return=representation brings the changed rows back, in the columns
// select names; without it nothing comes back, as PostgREST's default. count=
// answers a Content-Range with the number of rows changed. tx=rollback runs
// the write and rolls it back. A single-object Accept holds the write to one
// row, and rolls back any other.
//
// An update or a delete with no filter is refused, as PostgREST refuses it
// with pg-safeupdate loaded, which Supabase does: 21000, "requires a WHERE
// clause". A write runs as its caller's role, in one transaction, and the row
// rules decide; a refusal of the Database is passed on with its own message,
// code, details and hint, because that is what the SDK shows a developer, and
// the transaction leaves no partial change.
//
// The bounds are the read's, applied to what a write changes: at most
// MaxPageSize rows a write, counted as the rows its body holds and the rows
// its statement changes, and the statement time limit. Over the quota is a
// refusal of the whole write (MS104).

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"

	"github.com/lib/pq"
	"github.com/structy/log"

	"github.com/prest/prest/v2/adapters"
	"github.com/prest/prest/v2/internal/logsafe"
)

// timeLimitWriteMessage is timeLimitMessage for a write.
const timeLimitWriteMessage = "the write ran past the time limit and was cancelled; write fewer rows or add a filter"

// PostgRESTWrite serves POST, PATCH and DELETE on /rest/v1/{table}.
func (h *CRUDHandler) PostgRESTWrite(w http.ResponseWriter, r *http.Request) {
	table := pathVars(r)["table"]
	if err := pgrCheckName(table, "table"); err != nil {
		pgrWriteError(w, err)
		return
	}
	if len(r.URL.RawQuery) > h.bounds.MaxQueryLen {
		pgrWriteError(w, &pgrError{
			status:  http.StatusRequestURITooLong,
			code:    "MS101",
			message: queryTooLong,
			details: fmt.Sprintf("rest reads at most %d bytes of query string", h.bounds.MaxQueryLen),
			hint:    "send fewer filters, or a shorter in list",
		})
		return
	}
	if profile := r.Header.Get("Content-Profile"); profile != "" && profile != pgrSchema {
		pgrWriteError(w, pgrBadSchema(profile))
		return
	}
	if media, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); r.Method != http.MethodDelete && media != "" && media != "application/json" {
		pgrWriteError(w, pgrBadContentType(media))
		return
	}

	prefer := pgrParsePrefer(r.Header.Values("Prefer"))
	query := r.URL.Query()
	onConflict, columnsParam := query.Get("on_conflict"), query.Get("columns")
	query.Del("on_conflict")
	query.Del("columns")
	for _, key := range []string{"limit", "offset"} {
		if query.Has(key) {
			pgrWriteError(w, pgrRefuse("limit and offset on a write are not answered here yet"))
			return
		}
	}
	read, err := pgrParseRead(query, h.bounds.MaxPageSize)
	if err != nil {
		pgrWriteError(w, err)
		return
	}

	selected, err := pgrWriteSelect(read.items)
	if err != nil {
		pgrWriteError(w, err)
		return
	}
	m := &pgrMutation{
		table:          table,
		representation: prefer.representation(),
		headersOnly:    prefer.headersOnly(),
		selected:       selected,
		order:          read.order,
		quota:          h.bounds.MaxPageSize,
		defaults:       prefer.missingDefault(),
	}
	args := read.args
	switch r.Method {
	case http.MethodPost, http.MethodPatch:
		m.kind = pgrInsert
		if r.Method == http.MethodPatch {
			m.kind = pgrUpdate
			if len(read.where) == 0 {
				pgrWriteError(w, pgrNoFilter("UPDATE", "update"))
				return
			}
		}
		raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, int64(h.bounds.MaxBodyLen)))
		if err != nil {
			var tooLarge *http.MaxBytesError
			if errors.As(err, &tooLarge) {
				pgrWriteError(w, &pgrError{
					status:  http.StatusRequestEntityTooLarge,
					code:    "MS105",
					message: "the body is longer than rest reads",
					details: fmt.Sprintf("rest reads at most %d bytes of body", h.bounds.MaxBodyLen),
					hint:    "write fewer rows at a time",
				})
				return
			}
			pgrWriteError(w, &pgrError{status: http.StatusBadRequest, code: "PGRST102", message: "Empty or invalid json"})
			return
		}
		m.body, err = pgrParseBody(raw, h.bounds.MaxPageSize)
		if err != nil {
			pgrWriteError(w, err)
			return
		}
		if m.kind == pgrUpdate && m.body.isArray {
			pgrWriteError(w, &pgrError{status: http.StatusBadRequest, code: "PGRST102", message: "Empty or invalid json",
				details: "an update takes one JSON object"})
			return
		}
		m.bodyArg = len(args) + 1
		args = append(args, string(raw))
		if m.kind == pgrInsert {
			m.upsert = prefer.resolution != ""
			m.merge = prefer.resolution == "merge-duplicates"
			m.tracksInserted = m.merge
		}
	case http.MethodDelete:
		m.kind = pgrDelete
		if len(read.where) == 0 {
			pgrWriteError(w, pgrNoFilter("DELETE", "delete"))
			return
		}
	}
	m.where = strings.Join(read.where, " AND ")

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
	roles, _ := h.rolesOf(adapter)
	anon, _ := anonymousRole(roles, project)
	authed := caller.Role != anon && caller.Role != "anon"

	ctx, cancel := requestContext(r, project)
	defer cancel()

	// PostgREST finds the table and plans the write, columns included, and only
	// then checks the preferences and negotiates the media type, for every
	// write, one that returns nothing included (Plan.hs, mutateReadPlan). Both
	// are known now, since they need no Database, and raised in the
	// transaction once the table and its columns are found.
	media, early := pgrNegotiate(r.Header.Get("Accept"))
	if fault := prefer.strictFault(); fault != nil {
		early = fault
	}
	m.media = media
	wantsObject := media.object()
	var result pgrMutationResult
	run := func(writer adapters.CallerWriter) error {
		result = pgrMutationResult{}
		return writer.WriteAsCallerCtx(ctx, caller, func(tx *sql.Tx) error {
			schema, err := pgrLoadColumns(ctx, tx, table)
			if err != nil {
				return err
			}
			m.schema = schema
			if err := m.plan(ctx, tx, r.Method, columnsParam, onConflict); err != nil {
				return err
			}
			// An update that sets no column changes nothing, as PostgREST
			// answers it: an empty result.
			if early != nil {
				return early
			}
			if m.kind != pgrUpdate || len(m.written) > 0 {
				if result, err = m.run(ctx, tx, args); err != nil {
					return err
				}
			}
			if result.total > int64(m.quota) {
				return pgrWriteQuota(int(result.total), m.quota, "the write would change")
			}
			if wantsObject && result.total != 1 {
				return &pgrError{status: http.StatusNotAcceptable, code: "PGRST116",
					message: "Cannot coerce the result to a single JSON object",
					details: fmt.Sprintf("The result contains %d rows", result.total)}
			}
			if m.kind != pgrInsert && prefer.exceeds(result.total) {
				return pgrMaxAffectedFault(result.total)
			}
			if prefer.rollback() {
				return adapters.ErrRollback
			}
			return nil
		})
	}

	writer, ok := adapter.(adapters.CallerWriter)
	if !ok {
		pgrWriteError(w, &pgrError{status: http.StatusInternalServerError, code: "PGRST000", message: adapters.ErrRoleNotEntered.Error()})
		return
	}
	err = run(writer)
	// The one failure a fresh lookup fixes: the Database refused rest's own
	// login before it ran anything, so asking again repeats nothing (#548).
	if err != nil && credentialWasRotated(err) && h.admitter != nil {
		if fresh, freshErr := h.admitter.Readmit(ctx, project); freshErr == nil {
			if freshWriter, ok := fresh.(adapters.CallerWriter); ok {
				err = run(freshWriter)
			}
		}
	}
	if err != nil {
		log.Errorln(logsafe.Error(err))
		failure := pgrWriteFailure(err, authed)
		var pqErr *pq.Error
		if errors.As(err, &pqErr) && string(pqErr.Code) == undefinedTable {
			// A table the Database does not have is PostgREST's PGRST205.
			if missing := h.tableNotFound(ctx, adapter, caller, table); missing != nil {
				failure = missing
			}
		}
		pgrWriteError(w, failure)
		return
	}

	w.Header().Set("Content-Range", pgrWriteContentRange(m.kind, result.total, prefer.counted()))
	status := http.StatusNoContent
	switch {
	case m.kind == pgrInsert && m.merge && result.inserted == 0:
		status = http.StatusOK
	case m.kind == pgrInsert:
		status = http.StatusCreated
	case prefer.representation():
		status = http.StatusOK
	}
	prefer.setApplied(w, pgrApplied{
		resolution:     m.kind == pgrInsert && len(m.conflict) > 0,
		missing:        m.kind != pgrDelete,
		representation: true,
		maxAffected:    m.kind != pgrInsert,
	})
	if !prefer.representation() {
		if location := result.location(m.table, m.locate); location != "" {
			w.Header().Set("Location", location)
		}
		w.WriteHeader(status)
		return
	}
	body := []byte("[]")
	if len(result.rows) > 0 {
		body, _ = json.Marshal(result.rows)
	}
	w.Header().Set("Content-Type", m.media.contentType())
	switch {
	case wantsObject && len(result.rows) == 1:
		body = result.rows[0]
	case wantsObject:
		// An object asked for and none changed: the empty array, as before.
		w.Header().Set("Content-Type", pgrJSONMediaType)
	case !m.media.isJSON():
		body = []byte(result.text)
		if result.text == "" {
			body = []byte(m.media.empty())
		}
	}
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// pgrWriteSelect is the columns a write returns: plain names and `*`. An
// embedded table, an alias, a cast and a JSON path in a write's select are
// refused by name.
func pgrWriteSelect(items []pgrItem) ([]string, error) {
	var names []string
	for _, item := range items {
		field := item.field
		if field == nil || field.alias != "" || field.cast != "" || len(field.col.steps) > 0 {
			return nil, pgrRefuse("a write's select takes column names and * only")
		}
		if field.star {
			names = append(names, "*")
			continue
		}
		names = append(names, field.col.name)
	}
	return names, nil
}

// plan fills in what the catalog decides: the columns a body sets, and the
// columns a conflict is named by.
func (m *pgrMutation) plan(ctx context.Context, tx *sql.Tx, method, columnsParam, onConflict string) error {
	if m.kind == pgrDelete {
		return nil
	}
	names := m.body.keys
	if columnsParam != "" && m.kind == pgrInsert {
		names = nil
		for _, item := range pgrSplit(columnsParam) {
			names = append(names, strings.Trim(strings.TrimSpace(item), `"`))
		}
	}
	m.written = nil
	for _, name := range names {
		column, err := pgrSchemaColumn(m.table, m.schema, name)
		if err != nil {
			return err
		}
		m.written = append(m.written, column)
	}
	if m.kind == pgrInsert && (m.headersOnly || (m.upsert && onConflict == "")) {
		key, err := pgrLoadPrimaryKey(ctx, tx, m.table)
		if err != nil {
			return err
		}
		m.locate = pgrInTableOrder(key, m.schema)
		if m.upsert && onConflict == "" {
			m.conflict = key
		}
	}
	if m.kind != pgrInsert || !m.upsert {
		return nil
	}
	if onConflict != "" {
		m.conflict = nil
		for _, item := range pgrSplit(onConflict) {
			name := strings.Trim(strings.TrimSpace(item), `"`)
			if err := pgrCheckName(name, "column"); err != nil {
				return err
			}
			m.conflict = append(m.conflict, name)
		}
	}
	return nil
}

// pgrNoFilter is the refusal of an update or a delete that names no filter,
// in the words and the code pg-safeupdate raises.
func pgrNoFilter(verb, lower string) error {
	return &pgrError{
		status:  http.StatusBadRequest,
		code:    "21000",
		message: verb + " requires a WHERE clause",
		hint:    fmt.Sprintf(`To %s all rows in a table, use "WHERE true" or similar.`, lower),
	}
}

// pgrWriteContentRange is PostgREST's Content-Range for a write: the rows an
// update changed, `*` for an insert and a delete, and the total when a count
// was asked for.
func pgrWriteContentRange(kind pgrWriteKind, total int64, counted bool) string {
	all := "*"
	if counted {
		all = strconv.FormatInt(total, 10)
	}
	if kind == pgrUpdate && total > 0 && all != "0" {
		return fmt.Sprintf("0-%d/%s", total-1, all)
	}
	return "*/" + all
}

// pgrWriteFailure is a failed write in PostgREST's shape. Where the Database
// refused the write, the refusal's own message, SQLSTATE, details and hint are
// passed on, with the status PostgREST maps the SQLSTATE to: a developer needs
// them to tell a broken row rule from a broken constraint, and the caller is
// the one whose rows they describe.
func pgrWriteFailure(err error, authed bool) *pgrError {
	var refused *pgrError
	if errors.As(err, &refused) {
		return refused
	}
	if errors.Is(err, adapters.ErrRoleNotEntered) || errors.Is(err, adapters.ErrNoAnonymousRole) {
		return &pgrError{status: http.StatusInternalServerError, code: "PGRST000", message: adapters.ErrRoleNotEntered.Error()}
	}
	var pqErr *pq.Error
	if errors.As(err, &pqErr) {
		code := string(pqErr.Code)
		switch code {
		case invalidPassword, invalidAuthorizationSpecifier:
			return &pgrError{status: http.StatusBadGateway, code: "PGRST000", message: "the Database could not be written"}
		case queryCanceled:
			return &pgrError{status: http.StatusGatewayTimeout, code: queryCanceled, message: timeLimitWriteMessage,
				hint: "write fewer rows, or add a filter an index can answer"}
		}
		return &pgrError{
			status:  pgrDatabaseStatus(code, pqErr.Message, authed),
			code:    code,
			message: pqErr.Message,
			details: pqErr.Detail,
			hint:    pqErr.Hint,
		}
	}
	status, message := readFailure(err, "")
	switch status {
	case http.StatusGatewayTimeout:
		return &pgrError{status: status, code: queryCanceled, message: timeLimitWriteMessage,
			hint: "write fewer rows, or add a filter an index can answer"}
	case http.StatusBadGateway:
		return &pgrError{status: status, code: "PGRST000", message: "the Database could not be written"}
	}
	return &pgrError{status: http.StatusInternalServerError, code: "PGRST000", message: strings.Replace(message, "read", "write", 1)}
}

// pgrDatabaseStatus is the HTTP status PostgREST gives a SQLSTATE.
func pgrDatabaseStatus(code, message string, authed bool) int {
	switch {
	case strings.HasPrefix(code, "08"):
		return http.StatusServiceUnavailable
	case strings.HasPrefix(code, "09"):
		return http.StatusInternalServerError
	case strings.HasPrefix(code, "0L"), strings.HasPrefix(code, "0P"):
		return http.StatusForbidden
	case code == "23503", code == "23505":
		return http.StatusConflict
	case code == "25006":
		return http.StatusMethodNotAllowed
	case code == "21000":
		if strings.HasSuffix(message, "requires a WHERE clause") {
			return http.StatusBadRequest
		}
		return http.StatusInternalServerError
	case code == "22023":
		if strings.HasPrefix(message, "role") && strings.HasSuffix(message, "does not exist") {
			return http.StatusUnauthorized
		}
		return http.StatusBadRequest
	case code == "53400":
		return http.StatusInternalServerError
	case strings.HasPrefix(code, "53"):
		return http.StatusServiceUnavailable
	case code == "57P01":
		return http.StatusServiceUnavailable
	case code == "P0001":
		return http.StatusBadRequest
	case strings.HasPrefix(code, "PT"):
		// A function raises its own status: SQLSTATE PT402 answers 402
		// (Error.hs, pgErrorStatus).
		if n, err := strconv.Atoi(code[2:]); err == nil && n >= 100 && n <= 599 {
			return n
		}
		return http.StatusInternalServerError
	case code == "42883", code == "42P01":
		return http.StatusNotFound
	case code == "42P17":
		return http.StatusInternalServerError
	case code == "42501":
		if authed {
			return http.StatusForbidden
		}
		return http.StatusUnauthorized
	}
	for _, class := range []string{"25", "2D", "38", "39", "3B", "40", "54", "55", "57", "58", "F0", "HV", "P0", "XX"} {
		if strings.HasPrefix(code, class) {
			return http.StatusInternalServerError
		}
	}
	if strings.HasPrefix(code, "28") {
		return http.StatusForbidden
	}
	return http.StatusBadRequest
}
