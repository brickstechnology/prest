package controllers

import (
	"context"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"

	"github.com/prest/prest/v2/adapters"
	"github.com/prest/prest/v2/admission"
	pctx "github.com/prest/prest/v2/context"
	"github.com/prest/prest/v2/controllers/auth"
	"github.com/prest/prest/v2/internal/ident"
	"github.com/prest/prest/v2/internal/logsafe"
	"github.com/prest/prest/v2/middlewares"

	"github.com/lib/pq"
	"github.com/structy/log"
)

// CRUDHandler serves table CRUD endpoints.
type CRUDHandler struct {
	builder  adapters.RequestQueryBuilder
	sql      adapters.SQLBuilder
	executor adapters.QueryExecutor
	perms    adapters.PermissionsChecker
	db       adapters.DatabaseRegistry
	cache    ResponseCacher
	singleDB bool
	roles    adapters.AnonymousRoles
	reader   adapters.RoleReader
	// miniship: the Databases rest was given, one adapter each. Nil is
	// upstream's one-adapter shape, which the fields above serve.
	registry adapters.Registry
	// miniship: how a Project rest was not given is learned about, while rest
	// runs (#548). Nil is rest with no lookup at all, which answers such a
	// name 404 as #547 shipped it.
	admitter Admitter
	// miniship: the limits every table read is held to (#549).
	bounds QueryBounds
}

// servedSchema is the one schema rest serves (miniship). A Project's own
// tables are in public; every other schema is refused before any SQL.
const servedSchema = "public"

// NewCRUDHandler creates a CRUDHandler.
func NewCRUDHandler(deps Deps) *CRUDHandler {
	return &CRUDHandler{
		builder:  deps.Builder,
		sql:      deps.SQL,
		executor: deps.Executor,
		perms:    deps.Perms,
		db:       deps.DB,
		cache:    deps.Cache,
		singleDB: deps.SingleDB,
		roles:    deps.Roles,
		reader:   deps.Reader,
		registry: deps.AdapterRegistry,
		admitter: deps.Admitter,
		bounds:   deps.Bounds.withDefaults(),
	}
}

// Select performs a SELECT on a table.
//
// miniship: the read runs as the database's anonymous role, inside a
// read-only transaction that became it, over public alone. A database given
// with no role, or an adapter that cannot become one, is refused, and the read
// is never run as the login instead. pREST's own access list is not what
// protects a row here: rest runs with access.restrict off, so the database's
// grants and row security decide.
//
// The Database is chosen here, from the matched route, and the adapter holding
// it is what the read runs on (miniship-cloud#547).
func (h *CRUDHandler) Select(w http.ResponseWriter, r *http.Request) {
	vars := pathVars(r)
	database := vars["database"]
	schema := vars["schema"]
	table := vars["table"]

	// miniship: a database rest was not given is not found, as a route rest
	// does not have is not found. Upstream answers 400. A Project rest has
	// simply not seen yet is looked up here, once, and then it is a Database
	// rest was given (#548).
	adapter, err := h.databaseInPath(r.Context(), database)
	if err != nil {
		status, message := admissionFailure(err)
		jsonError(w, message, status)
		return
	}
	roles, reader := h.rolesOf(adapter)
	// miniship: the SQL is built by the adapter that holds this Database too,
	// and not by whichever one was registered first (#548). tableReference
	// qualifies a table with the Database's name unless the adapter it is
	// asked of carries a registry — so a rest that was given no Database and
	// learned about every one of them by asking built `"alpha"."public"."posts"`
	// on the default adapter, and Postgres answers a three-part reference with
	// "cross-database references are not implemented". #547 moved the reader
	// onto the matched Database's adapter; this is the builder following it.
	build := h.builderFor(adapter)

	if !validatePathSegments(database, schema, table) {
		jsonError(w, "invalid identifier in path", http.StatusBadRequest)
		return
	}

	// miniship: a schema that is not public is not found, as a database rest
	// was not given is not found, and no SQL is built for it.
	if schema != servedSchema {
		jsonError(w, fmt.Sprintf("schema not served: %v", schema), http.StatusNotFound)
		return
	}

	// miniship: the query string, reviewed, before anything is built and
	// before any connection is opened. query_screen.go is the second wall;
	// what the builders below read is the query it rebuilt, never the
	// caller's own (#549).
	if len(r.URL.RawQuery) > h.bounds.MaxQueryLen {
		jsonError(w, queryTooLong, http.StatusRequestURITooLong)
		return
	}
	screened, err := screenTableRead(r.URL.Query(), h.bounds)
	if err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	// asAsked is the request as it arrived, kept for the cache alone: the
	// cache middleware looks an answer up under that key on the way in, so
	// writing it under the screened one would never hit.
	asAsked := r
	r = withQuery(r, screened)
	queries := screened

	// miniship: the role, before anything is built. There is no default, and
	// it is this Database's own: the adapter chosen above answers for it.
	role, ok := anonymousRole(roles, database)
	if !ok || reader == nil {
		jsonError(w, adapters.ErrNoAnonymousRole.Error(), http.StatusInternalServerError)
		return
	}

	userInfo := r.Context().Value(pctx.UserInfoKey)
	var userName string
	if userInfo != nil {
		if user, ok := userInfo.(auth.User); ok {
			userName = user.Username
		}
	}

	cols, err := h.perms.FieldsPermissions(r, database, schema, table, "read", userName)
	if err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}

	if len(cols) == 0 {
		err := errors.New("you don't have permission for this action, please check the permitted fields for this table")
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}

	selectStr, err := build.SelectFields(cols)
	if err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	query := build.SelectSQL(selectStr, database, schema, table)

	distinct, err := h.builder.DistinctClause(r)
	if err != nil {
		err = fmt.Errorf("could not perform Distinct: %v", err)
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	if distinct != "" {
		query = strings.Replace(query, "SELECT", distinct, 1)
	}

	countQuery, err := h.builder.CountByRequest(r)
	if err != nil {
		err = fmt.Errorf("could not perform CountByRequest: %v", err)
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	countFirst := false
	if countQuery != "" {
		query = build.SelectSQL(countQuery, database, schema, table)
		if queries.Get("_count_first") != "" {
			countFirst = true
		}
	}

	joinValues, err := h.builder.JoinByRequest(r)
	if err != nil {
		err = fmt.Errorf("could not perform JoinByRequest: %v", err)
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}

	for _, j := range joinValues {
		query = fmt.Sprint(query, j)
	}

	requestWhere, values, err := h.builder.WhereByRequest(r, 1)
	if err != nil {
		err = fmt.Errorf("could not perform WhereByRequest: %v", err)
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	sqlSelect := query
	if requestWhere != "" {
		sqlSelect = fmt.Sprint(query, " WHERE ", requestWhere)
	}

	groupBySQL := h.builder.GroupByClause(r)
	if groupBySQL != "" {
		sqlSelect = fmt.Sprintf("%s %s", sqlSelect, groupBySQL)
	}

	timeBucketSQL, err := h.builder.TimeBucketClause(r)
	if err != nil {
		err = fmt.Errorf("could not perform TimeBucketClause: %w", err)
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	if timeBucketSQL != "" {
		if groupBySQL != "" {
			bucketExpr := strings.TrimSpace(strings.TrimPrefix(timeBucketSQL, "GROUP BY"))
			sqlSelect = fmt.Sprintf("%s, %s", sqlSelect, bucketExpr)
		} else {
			sqlSelect = fmt.Sprintf("%s %s", sqlSelect, timeBucketSQL)
		}
	}

	order, err := h.builder.OrderByRequest(r)
	if err != nil {
		err = fmt.Errorf("could not perform OrderByRequest: %v", err)
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	if order != "" {
		sqlSelect = fmt.Sprintf("%s %s", sqlSelect, order)
	}

	page, err := h.builder.PaginateIfPossible(r)
	if err != nil {
		err = fmt.Errorf("could not perform PaginateIfPossible: %v", err)
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	sqlSelect = fmt.Sprint(sqlSelect, " ", page)

	ctx, cancel := requestContext(r, database)
	defer cancel()

	read := func(reader adapters.RoleReader, role string) adapters.Scanner {
		if countFirst {
			return reader.QueryCountAsRoleCtx(ctx, role, sqlSelect, values...)
		}
		return reader.QueryAsRoleCtx(ctx, role, sqlSelect, values...)
	}

	sc := read(reader, role)
	// miniship (#548): the Database refusing rest's login, rather than
	// refusing the read, is the one failure a fresh answer can fix — a
	// credential that was rotated since rest was told it. It is worth exactly
	// one fresh lookup: the gate refuses a second one made too soon after it,
	// so a credential the answerer cannot fix is a failure and never a loop.
	//
	// Err is asked once for each read and its answer carried, because a
	// Scanner is not promised to be asked twice.
	err = sc.Err()
	if err != nil && credentialWasRotated(err) {
		if fresh, freshRole, ok := h.rotated(ctx, database); ok {
			sc = read(fresh, freshRole)
			err = sc.Err()
		}
	}
	if err != nil {
		// miniship (#549): the detail goes to the log, redacted, and never to
		// the caller. #545 found upstream answering with the driver's own
		// error, the Database's host and port included.
		log.Errorln(logsafe.Error(err))
		status, message := readFailure(err, table)
		jsonError(w, message, status)
		return
	}

	if r.Method == "GET" && h.cache != nil {
		h.cache.BuntSet(middlewares.CacheKey(asAsked), string(sc.Bytes()))
	}
	//nolint
	w.Write(sc.Bytes())
}

// The SQLSTATEs a table read turns into an answer of its own (miniship).
const (
	// insufficientPrivilege is a privilege the current role does not hold.
	insufficientPrivilege = "42501"
	// undefinedTable is a relation that is not there, or that the role cannot
	// see; queryCanceled is a statement stopped by statement_timeout.
	undefinedTable  = "42P01"
	undefinedColumn = "42703"
	queryCanceled   = "57014"
)

// readFailure is the answer a failed read gives: a status and a message that
// is the same for every caller and every Database (miniship, #549). Nothing
// here is built from the driver's words except the one message #546 kept —
// Postgres's own account of a privilege the anonymous role lacks, which names
// the relation and nothing about where the Database is.
func readFailure(err error, table string) (int, string) {
	switch {
	case errors.Is(err, adapters.ErrRoleNotEntered), errors.Is(err, adapters.ErrNoAnonymousRole):
		return http.StatusInternalServerError, adapters.ErrRoleNotEntered.Error()
	case errors.Is(err, context.DeadlineExceeded):
		return http.StatusGatewayTimeout, timeLimitMessage
	}
	var pqErr *pq.Error
	if errors.As(err, &pqErr) {
		switch pqErr.Code {
		case insufficientPrivilege:
			return http.StatusForbidden, pqErr.Message
		case undefinedTable:
			return http.StatusNotFound, "no such table"
		case undefinedColumn:
			return http.StatusBadRequest, "no such column"
		case queryCanceled:
			return http.StatusGatewayTimeout, timeLimitMessage
		}
		return http.StatusBadRequest, "the read could not be run"
	}
	// Upstream's own shape, kept so a Scanner that carries a plain error still
	// answers 404 for a relation that is not there.
	if strings.Contains(err.Error(), fmt.Sprintf(`pq: relation "%s.%s" does not exist`, servedSchema, table)) {
		return http.StatusNotFound, "no such table"
	}
	// The Database not answering: refused, unreachable, closed mid-read. What
	// the driver would have said is where it is, so none of it is repeated.
	var netErr net.Error
	if errors.As(err, &netErr) || errors.Is(err, driver.ErrBadConn) || errors.Is(err, io.EOF) {
		return http.StatusBadGateway, "the Database could not be read"
	}
	// Everything left is rest's own fault, not the Database's, and saying so
	// is the difference between an operator looking at the right thing and the
	// wrong one.
	return http.StatusInternalServerError, "the read could not be completed"
}

// timeLimitMessage is what a read cancelled by the time limit says. It is one
// message for both the statement's own cancellation and the request deadline,
// because a caller can do the same thing about either: ask for less.
const timeLimitMessage = "the read ran past the time limit and was cancelled; ask for fewer rows or add a filter"

// databaseInPath resolves the Database a request's path names to the adapter
// registered for it, and returns that adapter's role and reader (miniship).
//
// **One adapter holds one Database**: its pool, and the anonymous role reads of
// it become. Choosing it here, after the router has matched, is what makes one
// rest serve two Projects — and, with the adapter given that Database alone,
// what makes a Project's rows unreachable through another Project's
// connection. Upstream chose in AdapterSelectorMiddleware, which ran outside
// the router where mux.Vars is empty, so it never saw {database} and every
// read used whichever adapter the registry's map handed back first.
//
// A Database rest was not given has no adapter, and is not found.
//
// With no registry there is upstream's one adapter and upstream's checks on
// the name, pg.single among them. pg.single is about that shape: it asks
// whether the name is the one physical database this process connected to, and
// a registry answers that question by alias instead.
//
// A name with no adapter is a miss, and a miss is where rest learns about a
// Project it was not given (#548): it asks the process holding the Database
// plugin, once, for that Project alone. The registry is checked first and the
// check is the whole of the second call's cost, so a Project's second and
// later calls cause no lookup.
func (h *CRUDHandler) databaseInPath(ctx context.Context, database string) (adapters.Adapter, error) {
	if h.registry == nil {
		if err := validateDatabase(database, h.db, h.singleDB); err != nil {
			return nil, err
		}
		return nil, nil
	}
	adapter, err := h.registry.Get(database)
	if err != nil {
		return h.admit(ctx, database)
	}
	return adapter, nil
}

// admit looks a Project up, when rest has an answerer to ask and the name is
// one that could be a Project at all.
//
// The name is held to the path-segment rule before it is asked about, so what
// reaches the answerer is a name and not whatever arrived in the first segment
// of somebody's URL. Without an answerer this is #547's answer unchanged: a
// Database rest was not given, and no connection.
func (h *CRUDHandler) admit(ctx context.Context, database string) (adapters.Adapter, error) {
	if h.admitter == nil || !ident.IsSafeSegment(database) {
		return nil, admission.ErrNoSuchProject
	}
	return h.admitter.Admit(ctx, database)
}

// rotated asks for this Project once more and answers with the reader and the
// role of the pool the fresh credential opened. ok is false when there is no
// answerer, when it was asked too recently, or when what came back cannot be
// read with — and the read then reports the failure it already had.
func (h *CRUDHandler) rotated(ctx context.Context, database string) (adapters.RoleReader, string, bool) {
	if h.admitter == nil {
		return nil, "", false
	}
	adapter, err := h.admitter.Readmit(ctx, database)
	if err != nil {
		return nil, "", false
	}
	roles, reader := h.rolesOf(adapter)
	if reader == nil {
		return nil, "", false
	}
	role, ok := anonymousRole(roles, database)
	if !ok {
		return nil, "", false
	}
	return reader, role, true
}

// rolesOf is the role this Database's reads become and the reader that becomes
// it. A nil adapter is the no-registry shape, where there is one of each on the
// handler; an adapter that can do neither leaves both nil, and the read then
// refuses rather than reading as the login.
func (h *CRUDHandler) rolesOf(adapter adapters.Adapter) (adapters.AnonymousRoles, adapters.RoleReader) {
	if adapter == nil {
		return h.roles, h.reader
	}
	roles, _ := adapter.(adapters.AnonymousRoles)
	reader, _ := adapter.(adapters.RoleReader)
	return roles, reader
}

// builderFor is the SQL builder for this Database. It matters because
// tableReference asks the adapter it is called on whether that adapter carries
// a registry, and answers a three-part, cross-database reference when it does
// not — so the builder has to be this Database's own, not the handler's.
func (h *CRUDHandler) builderFor(adapter adapters.Adapter) adapters.SQLBuilder {
	if builder, ok := adapter.(adapters.SQLBuilder); ok {
		return builder
	}
	return h.sql
}

// anonymousRole returns the role reads of database become.
func anonymousRole(roles adapters.AnonymousRoles, database string) (string, bool) {
	if roles == nil {
		return "", false
	}
	return roles.AnonymousRole(database)
}

// Insert performs an INSERT on a table.
func (h *CRUDHandler) Insert(w http.ResponseWriter, r *http.Request) {
	vars := pathVars(r)
	database := vars["database"]
	schema := vars["schema"]
	table := vars["table"]

	if err := validateDatabase(database, h.db, h.singleDB); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}

	if !validatePathSegments(database, schema, table) {
		jsonError(w, "invalid identifier in path", http.StatusBadRequest)
		return
	}

	names, placeholders, values, err := h.builder.ParseInsertRequest(r)
	if err != nil {
		err = fmt.Errorf("could not perform InsertInTables: %v", err)
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}

	sql := h.sql.InsertSQL(database, schema, table, names, placeholders)

	ctx, cancel := requestContext(r, database)
	defer cancel()

	sc := h.executor.InsertCtx(ctx, sql, values...)
	if err = sc.Err(); err != nil {
		if strings.Contains(err.Error(), fmt.Sprintf(`pq: relation "%s.%s" does not exist`, schema, table)) {
			err = fmt.Errorf("relation does not exist: %v", err)
			jsonError(w, err.Error(), http.StatusNotFound)
			return
		}
		err = fmt.Errorf("could not perform InsertInTables: %v", err)
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.WriteHeader(http.StatusCreated)
	w.Write(sc.Bytes())
}

// BatchInsert performs a batch INSERT on a table.
func (h *CRUDHandler) BatchInsert(w http.ResponseWriter, r *http.Request) {
	vars := pathVars(r)
	database := vars["database"]
	schema := vars["schema"]
	table := vars["table"]

	if err := validateDatabase(database, h.db, h.singleDB); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}

	if !validatePathSegments(database, schema, table) {
		jsonError(w, "invalid identifier in path", http.StatusBadRequest)
		return
	}

	names, placeholders, values, err := h.builder.ParseBatchInsertRequest(r)
	if err != nil {
		err = fmt.Errorf("could not perform BatchInsertInTables: %v", err)
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}

	ctx, cancel := requestContext(r, database)
	defer cancel()

	var sc adapters.Scanner
	method := r.Header.Get("Prest-Batch-Method")
	if strings.ToLower(method) != "copy" {
		sql := h.sql.InsertSQL(database, schema, table, names, placeholders)
		sc = h.executor.BatchInsertValuesCtx(ctx, sql, values...)
	} else {
		sc = h.executor.BatchInsertCopyCtx(ctx, database, schema, table, strings.Split(names, ","), values...)
	}
	if err = sc.Err(); err != nil {
		if strings.Contains(err.Error(), fmt.Sprintf(`pq: relation "%s.%s" does not exist`, schema, table)) {
			err = fmt.Errorf("relation does not exist: %v", err)
			jsonError(w, err.Error(), http.StatusNotFound)
			return
		}
		err = fmt.Errorf("could not perform BatchInsertInTables: %v", err)
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.WriteHeader(http.StatusCreated)
	w.Write(sc.Bytes())
}

// Delete performs a DELETE on a table.
func (h *CRUDHandler) Delete(w http.ResponseWriter, r *http.Request) {
	vars := pathVars(r)
	database := vars["database"]
	schema := vars["schema"]
	table := vars["table"]

	if err := validateDatabase(database, h.db, h.singleDB); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}

	if !validatePathSegments(database, schema, table) {
		jsonError(w, "invalid identifier in path", http.StatusBadRequest)
		return
	}

	where, values, err := h.builder.WhereByRequest(r, 1)
	if err != nil {
		err = fmt.Errorf("could not perform WhereByRequest: %v", err)
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}

	sql := h.sql.DeleteSQL(database, schema, table)
	if where != "" {
		sql = fmt.Sprint(sql, " WHERE ", where)
	}

	returningSyntax, err := h.builder.ReturningByRequest(r)
	if err != nil {
		err = fmt.Errorf("could not perform ReturningByRequest: %v", err)
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}

	if returningSyntax != "" {
		sql = fmt.Sprint(sql, " RETURNING ", returningSyntax)
	}

	ctx, cancel := requestContext(r, database)
	defer cancel()

	sc := h.executor.DeleteCtx(ctx, sql, values...)
	if err = sc.Err(); err != nil {
		if strings.Contains(err.Error(), fmt.Sprintf(`pq: relation "%s.%s" does not exist`, schema, table)) {
			err = fmt.Errorf("relation does not exist: %v", err)
			jsonError(w, err.Error(), http.StatusNotFound)
			return
		}
		err = fmt.Errorf("could not perform DeleteFromTable: %v", err)
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.Write(sc.Bytes())
}

// Update performs an UPDATE on a table.
func (h *CRUDHandler) Update(w http.ResponseWriter, r *http.Request) {
	vars := pathVars(r)
	database := vars["database"]
	schema := vars["schema"]
	table := vars["table"]

	if err := validateDatabase(database, h.db, h.singleDB); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}

	if !validatePathSegments(database, schema, table) {
		jsonError(w, "invalid identifier in path", http.StatusBadRequest)
		return
	}

	setSyntax, values, err := h.builder.SetByRequest(r, 1)
	if err != nil {
		err = fmt.Errorf("could not perform UPDATE: %v", err)
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	sql := h.sql.UpdateSQL(database, schema, table, setSyntax)

	pid := len(values) + 1

	where, whereValues, err := h.builder.WhereByRequest(r, pid)
	if err != nil {
		err = fmt.Errorf("could not perform WhereByRequest: %v", err)
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}

	if where != "" {
		sql = fmt.Sprint(sql, " WHERE ", where)
		values = append(values, whereValues...)
	}

	returningSyntax, err := h.builder.ReturningByRequest(r)
	if err != nil {
		err = fmt.Errorf("could not perform ReturningByRequest: %v", err)
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}

	if returningSyntax != "" {
		sql = fmt.Sprint(sql, " RETURNING ", returningSyntax)
	}
	ctx, cancel := requestContext(r, database)
	defer cancel()

	sc := h.executor.UpdateCtx(ctx, sql, values...)
	if err = sc.Err(); err != nil {
		if strings.Contains(err.Error(), fmt.Sprintf(`pq: relation "%s.%s" does not exist`, schema, table)) {
			jsonError(w, err.Error(), http.StatusNotFound)
			return
		}
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.Write(sc.Bytes())
}
