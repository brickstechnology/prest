package controllers

// miniship (miniship-cloud#1304, public ADR 0077): GET, HEAD and POST on
// /rest/v1/rpc/{function}, the address the Supabase SDK's rpc() calls. They
// follow PostgREST v12.2.3 (the version the SDK at 2.117.3 expects), whose
// source each choice below cites.
//
//   - POST sends the arguments as a JSON object. GET and HEAD send them as
//     query parameters, every one that is not a filter: a value that begins
//     with an operator (`gt.2`, `not.in.(1,2)`) is a filter on the function's
//     result, any other is an argument (QueryParams.hs, parse with
//     isRpcRead). The Database is told the arguments by name, so the call is
//     `fn(a := 1, b := 2)` and an argument left out takes its default.
//   - The function is the overload whose argument names the call sent
//     (Plan.hs, findProc), as the caller's role, with the caller's claims in
//     the transaction. A function with no match is PGRST202, an ambiguous one
//     PGRST203.
//   - What comes back follows what the function returns (Statements.hs,
//     prepareCall, and SqlFragment.hs, asJsonF): a scalar is the JSON value,
//     a single row of a table or of OUT arguments an object, a set an array
//     of values or of objects, and void is 204. A set takes the read's filters,
//     order, limit and offset, and is cut at the read's page, as a table
//     read is (MS100 above it); the function runs once, in a CTE.
//   - GET and HEAD run in a read-only transaction, so a function that writes
//     is refused by the Database (25006, 405), which is how PostgREST refuses
//     it: Plan.hs gives every InvRead the read mode, and RpcSpec.hs's "only for
//     GET rpc" expects 405 of a function that mutates. POST runs in a
//     read-write transaction that commits only on success.
//   - A VOLATILE function's writes are counted against the write quota (MS104)
//     by the Database's own per-transaction row counters, before and after the
//     call, and a call over the quota is rolled back whole.
//
// Not answered, and refused by name: an embedded table in a function's select,
// `columns`, a body that is not JSON, and a JSON array as the arguments.

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"

	"github.com/structy/log"

	"github.com/prest/prest/v2/adapters"
	"github.com/prest/prest/v2/internal/logsafe"
)

// pgrFilterValue is the start of a value that is an operator rather than an
// argument: `not.`, then an operator of pOpExpr (QueryParams.hs), then `.`.
var pgrFilterValue = regexp.MustCompile(`^(not\.)?(isdistinct|imatch|plfts|phfts|wfts|fts|ilike|like|match|neq|nxr|nxl|adj|gte|gt|lte|lt|eq|in|is|cs|cd|ov|sl|sr)(\([A-Za-z0-9_]+\))?\.`)

// pgrCallKeyIsRead is whether a query key belongs to the function's result
// rather than to a filter or an argument: the keys QueryParams.hs reserves.
func pgrCallKeyIsRead(key string) bool {
	if key == "select" || key == "columns" || key == "on_conflict" {
		return true
	}
	words := strings.Split(key, ".")
	switch words[len(words)-1] {
	case "order", "limit", "offset", "and", "or":
		return true
	}
	return false
}

// pgrCallArgument is one argument sent in a query string.
type pgrCallArgument struct{ name, value string }

// pgrSplitCallQuery separates a GET's query string into what shapes the
// result and the arguments of the call.
func pgrSplitCallQuery(query url.Values) (url.Values, []pgrCallArgument) {
	shaped := url.Values{}
	var arguments []pgrCallArgument
	keys := make([]string, 0, len(query))
	for key := range query {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		for _, value := range query[key] {
			if pgrCallKeyIsRead(key) || pgrFilterValue.MatchString(value) {
				shaped.Add(key, value)
				continue
			}
			arguments = append(arguments, pgrCallArgument{key, value})
		}
	}
	return shaped, arguments
}

// pgrCallBody is a POST's body, looked at: the names it sends and whether it
// is an object.
type pgrCallBody struct {
	raw      []byte
	keys     map[string]bool
	isObject bool
}

// pgrParseCallBody reads the body of a call. An empty body is {} as
// PostgREST reads it (ApiRequest.hs, getPayload); anything that is not JSON is
// PGRST102. The keys are the object's, or the first object's of an array,
// which is how payloadAttributes names them.
func pgrParseCallBody(raw []byte) (*pgrCallBody, error) {
	invalid := &pgrError{status: http.StatusBadRequest, code: "PGRST102", message: "Empty or invalid json"}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		trimmed = []byte("{}")
	}
	if !json.Valid(trimmed) {
		return nil, invalid
	}
	body := &pgrCallBody{raw: trimmed, keys: map[string]bool{}}
	var first json.RawMessage
	switch trimmed[0] {
	case '{':
		body.isObject = true
		first = trimmed
	case '[':
		var items []json.RawMessage
		if err := json.Unmarshal(trimmed, &items); err != nil {
			return nil, invalid
		}
		if len(items) > 0 {
			first = items[0]
		}
	}
	if first != nil {
		if keys, err := pgrObjectKeys(first); err == nil {
			for _, key := range keys {
				body.keys[key] = true
			}
		}
	}
	return body, nil
}

// pgrCallPlan is a call, planned: the function, the arguments it is given and
// how the result is read.
type pgrCallPlan struct {
	routine *pgrRoutine
	read    *pgrRead
	// specified are the arguments given by name, in the function's order.
	specified []pgrParam
	// one is the function's only argument, which is given the whole body.
	one *pgrParam
	// arguments is the JSON the arguments are read from; empty when the call
	// gives none.
	arguments string
}

// pgrPlanCall chooses how the function is given its arguments, as Plan.hs's
// callPlan does.
func pgrPlanCall(routine *pgrRoutine, read *pgrRead, keys map[string]bool, singleObject bool, arguments string) *pgrCallPlan {
	plan := &pgrCallPlan{routine: routine, read: read, arguments: arguments}
	if len(routine.params) == 1 && (singleObject || routine.params[0].name == "") {
		plan.one = &routine.params[0]
		return plan
	}
	for _, p := range routine.params {
		if keys[p.name] {
			plan.specified = append(plan.specified, p)
		}
	}
	return plan
}

// statement is the whole call as one statement: the function runs once, in the
// CTE, and the page and the count are read from it. The first column is the
// page as a JSON array of what the function returned; the second the count
// when one is asked for. Everything that comes from the caller is a
// parameter: a name passed pgrIdent, and the argument names and types come
// from the catalog and are quoted.
func (c *pgrCallPlan) statement(counted bool) (string, []any, error) {
	return c.statementAs(counted, pgrMedia{})
}

// statementAs is statement with the page read as the body media asks for. A
// body that is not a JSON array (CSV, GeoJSON) comes with a third column, how
// many rows the page has.
func (c *pgrCallPlan) statementAs(counted bool, media pgrMedia) (string, []any, error) {
	read := c.read
	args := append([]any(nil), read.args...)
	arg := func(cast string) string {
		args = append(args, c.arguments)
		return fmt.Sprintf("$%d%s", len(args), cast)
	}

	var call strings.Builder
	var from, argList string
	switch {
	case c.one != nil:
		argList = arg("::" + c.one.typ)
	default:
		parts := make([]string, 0, len(c.specified))
		columns := make([]string, 0, len(c.specified))
		for _, p := range c.specified {
			prefix := ""
			if p.variadic {
				prefix = "VARIADIC "
			}
			parts = append(parts, prefix+pgrQuote(p.name)+" := "+pgrQuote("pgrst_body")+"."+pgrQuote(p.name))
			columns = append(columns, pgrQuote(p.name)+" "+p.typ)
		}
		argList = strings.Join(parts, ", ")
		if len(c.specified) > 0 {
			from = "json_to_record(" + arg("::json") + ") AS " + pgrQuote("pgrst_body") + "(" + strings.Join(columns, ", ") + "), LATERAL "
		}
	}
	function := pgrTable(c.routine.name) + "(" + argList + ")"
	call.WriteString("SELECT ")
	if c.routine.scalar() {
		call.WriteString(pgrQuote("pgrst_call") + "." + pgrQuote("pgrst_scalar") + " FROM " + from +
			"(SELECT " + function + " AS " + pgrQuote("pgrst_scalar") + ") AS " + pgrQuote("pgrst_call"))
	} else {
		call.WriteString(pgrQuote("pgrst_call") + ".* FROM " + from + function + " AS " + pgrQuote("pgrst_call"))
	}

	fields, err := read.fieldsSQL(read.items, "t")
	if err != nil {
		return "", nil, err
	}
	source := pgrQuote("pgrst_source")
	var page, pageRows string
	switch c.routine.result {
	case pgrResultVoid:
		// The call is read once so that it runs, and nothing is shown of it.
		page = fmt.Sprintf(`(SELECT '[]' WHERE (SELECT count(*) FROM %s) >= 0)`, source)
	default:
		inner := fmt.Sprintf(`SELECT %s FROM %s AS "t"%s%s LIMIT %d OFFSET %d`,
			fields, source, read.whereSQL(), read.order, read.limit, read.offset)
		page = "(" + media.aggregate(inner, `"x"`, c.routine.scalar()) + ")"
		if !media.isJSON() {
			pageRows = fmt.Sprintf(", (SELECT count(*) FROM (%s) AS \"n\")", inner)
		}
	}
	count := "NULL::bigint"
	if counted {
		count = fmt.Sprintf(`(SELECT count(*) FROM %s AS "t"%s)`, source, read.whereSQL())
	}
	return fmt.Sprintf("WITH %s AS (%s) SELECT %s, %s%s", source, call.String(), page, count, pageRows), args, nil
}

// pgrCallResult is what a call answered.
type pgrCallResult struct {
	rows  []json.RawMessage
	total int64
	// text and pageRows are the body and its row count when the media type is
	// not a JSON array.
	text     string
	pageRows int64
}

// pgrChangedRows is the rows the current transaction has inserted, updated and
// deleted so far, as the Database counts them (pg_stat_xact_user_tables). A
// function's writes are the difference before and after its call.
func pgrChangedRows(ctx context.Context, tx *sql.Tx) (int64, error) {
	var changed int64
	err := tx.QueryRowContext(ctx,
		`SELECT coalesce(sum(n_tup_ins + n_tup_upd + n_tup_del), 0)::bigint FROM pg_catalog.pg_stat_xact_user_tables`).Scan(&changed)
	return changed, err
}

// pgrCallQuota is the refusal of a call whose function changed more rows than
// a write may: the write quota, MS104.
func pgrCallQuota(rows int64, quota int) error {
	return &pgrError{
		status:  400,
		code:    "MS104",
		message: "the write changes more rows than rest allows at once",
		details: fmt.Sprintf("the function changed %d rows; rest changes at most %d rows a write", rows, quota),
		hint:    fmt.Sprintf("call the function so that it changes %d or fewer rows", quota),
	}
}

// pgrCallFailure is a failed call in PostgREST's shape. The function's own
// refusal is passed on with its message, SQLSTATE, details and hint, as a
// write's is, because a function raises its own errors for the caller to read.
func pgrCallFailure(err error, authed bool) *pgrError {
	status, _ := readFailure(err, "")
	if status == http.StatusGatewayTimeout {
		return &pgrError{status: status, code: queryCanceled, message: "the call ran past the time limit and was cancelled",
			hint: "ask for fewer rows, or add a filter an index can answer"}
	}
	return pgrWriteFailure(err, authed)
}

// PostgRESTCall serves GET, HEAD and POST on /rest/v1/rpc/{function}.
func (h *CRUDHandler) PostgRESTCall(w http.ResponseWriter, r *http.Request) {
	name := pathVars(r)["function"]
	if err := pgrCheckName(name, "function"); err != nil {
		pgrWriteError(w, err)
		return
	}
	isPost := r.Method == http.MethodPost
	if !isPost && r.Method != http.MethodGet && r.Method != http.MethodHead {
		pgrWriteError(w, &pgrError{status: http.StatusMethodNotAllowed, code: "PGRST101",
			message: "Cannot use the " + r.Method + " method on RPC"})
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
	profileHeader := "Accept-Profile"
	if isPost {
		profileHeader = "Content-Profile"
	}
	if profile := r.Header.Get(profileHeader); profile != "" && profile != pgrSchema {
		pgrWriteError(w, &pgrError{
			status:  http.StatusNotAcceptable,
			code:    "PGRST106",
			message: "The schema must be one of the following: " + pgrSchema,
		})
		return
	}
	if media, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); isPost && media != "" && media != "application/json" {
		pgrWriteError(w, &pgrError{
			status:  http.StatusUnsupportedMediaType,
			code:    "PGRST107",
			message: "None of these Content-Types are available: " + media,
		})
		return
	}

	prefer := pgrParsePrefer(r.Header.Values("Prefer"))
	query := r.URL.Query()
	if query.Has("columns") {
		pgrWriteError(w, pgrRefuse("columns on a function call is not answered here yet"))
		return
	}
	query.Del("on_conflict")
	var sent []pgrCallArgument
	if !isPost {
		query, sent = pgrSplitCallQuery(query)
	}
	read, err := pgrParseRead(query, h.bounds.MaxPageSize)
	if err != nil {
		pgrWriteError(w, err)
		return
	}
	if read.hasEmbeds() {
		pgrWriteError(w, pgrRefuse("an embedded table in a function's select is not answered here yet"))
		return
	}

	keys := map[string]bool{}
	var body *pgrCallBody
	if isPost {
		raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, int64(h.bounds.MaxBodyLen)))
		if err != nil {
			var tooLarge *http.MaxBytesError
			if errors.As(err, &tooLarge) {
				pgrWriteError(w, &pgrError{
					status:  http.StatusRequestEntityTooLarge,
					code:    "MS105",
					message: "the body is longer than rest reads",
					details: fmt.Sprintf("rest reads at most %d bytes of body", h.bounds.MaxBodyLen),
					hint:    "send fewer arguments",
				})
				return
			}
			pgrWriteError(w, &pgrError{status: http.StatusBadRequest, code: "PGRST102", message: "Empty or invalid json"})
			return
		}
		if body, err = pgrParseCallBody(raw); err != nil {
			pgrWriteError(w, err)
			return
		}
		keys = body.keys
	}
	for _, argument := range sent {
		keys[argument.name] = true
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
	roles, _ := h.rolesOf(adapter)
	anon, _ := anonymousRole(roles, project)
	authed := caller.Role != anon && caller.Role != "anon"

	ctx, cancel := requestContext(r, project)
	defer cancel()

	// PostgREST finds the function first, then negotiates the media type, then
	// checks the preferences (Plan.hs, callReadPlan): known now, raised in the
	// transaction once the function is found.
	media, early := pgrNegotiate(r.Header.Get("Accept"))
	if early == nil {
		early = prefer.strictFault()
	}
	wantsObject := media.object()
	var routine *pgrRoutine
	var result pgrCallResult
	exec := func(tx *sql.Tx) error {
		result = pgrCallResult{}
		found, err := pgrLoadRoutines(ctx, tx, name)
		if err != nil {
			return err
		}
		var routines []pgrRoutine
		for _, f := range found {
			if f.callable() {
				routines = append(routines, f)
			}
		}
		chosen, candidates, err := pgrFindRoutine(name, routines, keys, prefer.singleObject, isPost)
		switch {
		case errors.Is(err, errNoRoutine):
			var others []string
			if len(routines) == 0 {
				if others, err = pgrLoadFunctionNames(ctx, tx); err != nil {
					return err
				}
			}
			names := make([]string, 0, len(keys))
			for key := range keys {
				names = append(names, key)
			}
			return pgrNoRoutine(name, names, prefer.singleObject, isPost, routines, others)
		case err != nil:
			return pgrAmbiguousRoutine(candidates)
		}
		routine = chosen
		if early != nil {
			return early
		}
		if prefer.strict() && prefer.maxAffectedSet && chosen.single() {
			return &pgrError{status: http.StatusBadRequest, code: "PGRST128",
				message: "Function must return SETOF or TABLE when max-affected preference is used with handling=strict"}
		}

		arguments := ""
		if isPost {
			arguments = string(body.raw)
		} else if prefer.singleObject || len(sent) > 0 {
			arguments = pgrQueryArguments(chosen, sent)
		}
		plan := pgrPlanCall(chosen, read, keys, prefer.singleObject, arguments)
		if isPost && plan.one == nil && len(plan.specified) > 0 && !body.isObject {
			return &pgrError{status: http.StatusBadRequest, code: "PGRST102", message: "Empty or invalid json",
				details: "the arguments of a call are one JSON object"}
		}
		if plan.one == nil && len(plan.specified) == 0 {
			plan.arguments = ""
		}
		statement, args, err := plan.statementAs(prefer.counted(), media)
		if err != nil {
			return err
		}

		counts := isPost && chosen.volatility == 'v'
		var before int64
		if counts {
			if before, err = pgrChangedRows(ctx, tx); err != nil {
				return err
			}
		}
		var page string
		var total sql.NullInt64
		if media.isJSON() || chosen.result == pgrResultVoid {
			if err := tx.QueryRowContext(ctx, statement, args...).Scan(&page, &total); err != nil {
				return err
			}
			if err := json.Unmarshal([]byte(page), &result.rows); err != nil {
				return err
			}
		} else {
			if err := tx.QueryRowContext(ctx, statement, args...).Scan(&result.text, &total, &result.pageRows); err != nil {
				return err
			}
		}
		result.total = total.Int64
		if counts {
			after, err := pgrChangedRows(ctx, tx)
			if err != nil {
				return err
			}
			if after-before > int64(h.bounds.MaxPageSize) {
				return pgrCallQuota(after-before, h.bounds.MaxPageSize)
			}
		}
		returned := int64(len(result.rows))
		if chosen.single() {
			returned = 1
		}
		if wantsObject && !chosen.single() && len(result.rows) != 1 {
			return &pgrError{status: http.StatusNotAcceptable, code: "PGRST116",
				message: "Cannot coerce the result to a single JSON object",
				details: fmt.Sprintf("The result contains %d rows", len(result.rows))}
		}
		if prefer.exceeds(returned) {
			return pgrMaxAffectedFault(returned)
		}
		if isPost && prefer.rollback() {
			return adapters.ErrRollback
		}
		return nil
	}

	var run func(adapters.Adapter) error
	if isPost {
		run = func(a adapters.Adapter) error {
			writer, ok := a.(adapters.CallerWriter)
			if !ok {
				return adapters.ErrRoleNotEntered
			}
			return writer.WriteAsCallerCtx(ctx, caller, exec)
		}
	} else {
		run = func(a adapters.Adapter) error {
			reader, ok := a.(adapters.CallerReader)
			if !ok {
				return adapters.ErrRoleNotEntered
			}
			return reader.ReadAsCallerCtx(ctx, caller, exec)
		}
	}
	err = run(adapter)
	// The one failure a fresh lookup fixes, asked once, as a read asks it (#548).
	if err != nil && credentialWasRotated(err) && h.admitter != nil {
		if fresh, freshErr := h.admitter.Readmit(ctx, project); freshErr == nil {
			err = run(fresh)
		}
	}
	if err != nil {
		log.Errorln(logsafe.Error(err))
		pgrWriteError(w, pgrCallFailure(err, authed))
		return
	}

	returned := len(result.rows)
	if !media.isJSON() {
		returned = int(result.pageRows)
	}
	if routine.single() {
		returned = 1
	}
	w.Header().Set("Content-Range", pgrContentRange(read.offset, returned, result.total, prefer.counted()))
	prefer.setApplied(w, pgrApplied{maxAffected: true})
	if routine.result == pgrResultVoid {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	var out []byte
	switch {
	case !media.isJSON():
		out = []byte(result.text)
		w.Header().Set("Content-Type", media.contentType())
	case routine.single():
		out = []byte("null")
		if len(result.rows) > 0 {
			out = result.rows[0]
		}
		w.Header().Set("Content-Type", media.contentType())
	case wantsObject:
		out = result.rows[0]
		w.Header().Set("Content-Type", media.contentType())
	default:
		out = []byte("[]")
		if len(result.rows) > 0 {
			out, _ = json.Marshal(result.rows)
		}
		w.Header().Set("Content-Type", media.contentType())
	}
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = w.Write(out)
	}
}

// pgrQueryArguments is a GET's arguments as the JSON object a POST would send
// (CallPlan.hs, jsonRpcParams): the last value sent for a name, and for a
// variadic argument every value, in the order sent.
func pgrQueryArguments(routine *pgrRoutine, sent []pgrCallArgument) string {
	variadic := map[string]bool{}
	for _, p := range routine.params {
		if p.variadic {
			variadic[p.name] = true
		}
	}
	values := map[string]any{}
	for _, argument := range sent {
		if !variadic[argument.name] {
			values[argument.name] = argument.value
			continue
		}
		list, _ := values[argument.name].([]string)
		values[argument.name] = append(list, argument.value)
	}
	encoded, _ := json.Marshal(values)
	return string(encoded)
}
