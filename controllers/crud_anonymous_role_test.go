package controllers

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/golang/mock/gomock"
	"github.com/lib/pq"
	"github.com/stretchr/testify/require"

	"github.com/prest/prest/v2/adapters"
	"github.com/prest/prest/v2/adapters/mockgen"
	"github.com/prest/prest/v2/adapters/scanner"
)

// miniship: the table read runs as the database's anonymous role, and only
// over public. These drive CRUDHandler.Select with fakes that record every read
// asked of them, so "refused before any SQL" is a read that never arrived.

// staticRoles is a configuration naming one role for each alias it holds.
type staticRoles map[string]string

func (s staticRoles) AnonymousRole(alias string) (string, bool) {
	role, ok := s[alias]
	return role, ok && role != ""
}

// recordingReader is a RoleReader that records each read and answers with
// the scanner it was given.
type recordingReader struct {
	mu     sync.Mutex
	reads  []recordedRead
	answer adapters.Scanner
}

type recordedRead struct {
	count bool
	role  string
	sql   string
}

func (r *recordingReader) QueryAsRoleCtx(_ context.Context, role, SQL string, _ ...interface{}) adapters.Scanner {
	return r.record(false, role, SQL)
}

func (r *recordingReader) QueryCountAsRoleCtx(_ context.Context, role, SQL string, _ ...interface{}) adapters.Scanner {
	return r.record(true, role, SQL)
}

func (r *recordingReader) record(count bool, role, SQL string) adapters.Scanner {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.reads = append(r.reads, recordedRead{count: count, role: role, sql: SQL})
	return r.answer
}

func (r *recordingReader) recorded() []recordedRead {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]recordedRead(nil), r.reads...)
}

func answering(body string, err error) adapters.Scanner {
	return &scanner.PrestScanner{Error: err, Buff: bytes.NewBufferString(body), IsQuery: true}
}

// plainReadMocks are the builder and SQL mocks a read with no query string
// asks of, answering as the postgres adapter would.
func plainReadMocks(ctrl *gomock.Controller, schema, table string) (*mockgen.MockPermissionsChecker, *mockgen.MockSQLBuilder, *mockgen.MockRequestQueryBuilder) {
	perms := mockgen.NewMockPermissionsChecker(ctrl)
	perms.EXPECT().FieldsPermissions(gomock.Any(), "prest-test", schema, table, "read", "").Return([]string{"*"}, nil).AnyTimes()

	sqlBuilder := mockgen.NewMockSQLBuilder(ctrl)
	sqlBuilder.EXPECT().SelectFields([]string{"*"}).Return(`SELECT * FROM`, nil).AnyTimes()
	sqlBuilder.EXPECT().SelectSQL(gomock.Any(), "prest-test", schema, table).
		DoAndReturn(func(sel, _, s, t string) string { return fmt.Sprintf(`%s "%s"."%s"`, sel, s, t) }).AnyTimes()

	builder := mockgen.NewMockRequestQueryBuilder(ctrl)
	builder.EXPECT().DistinctClause(gomock.Any()).Return("", nil).AnyTimes()
	builder.EXPECT().CountByRequest(gomock.Any()).Return("", nil).AnyTimes()
	builder.EXPECT().JoinByRequest(gomock.Any()).Return(nil, nil).AnyTimes()
	builder.EXPECT().WhereByRequest(gomock.Any(), 1).Return("", nil, nil).AnyTimes()
	builder.EXPECT().GroupByClause(gomock.Any()).Return("").AnyTimes()
	builder.EXPECT().TimeBucketClause(gomock.Any()).Return("", nil).AnyTimes()
	builder.EXPECT().OrderByRequest(gomock.Any()).Return("", nil).AnyTimes()
	builder.EXPECT().PaginateIfPossible(gomock.Any()).Return("", nil).AnyTimes()
	return perms, sqlBuilder, builder
}

// anonymousRead builds a CRUDHandler for prest-test whose reads go to reader.
// The upstream executor is a mock with no expectations, so a read that went
// to it instead, as the login, fails the test.
func anonymousRead(t *testing.T, roles adapters.AnonymousRoles, reader adapters.RoleReader, schema string) *CRUDHandler {
	t.Helper()
	ctrl := gomock.NewController(t)
	perms, sqlBuilder, builder := plainReadMocks(ctrl, schema, "posts")
	return NewCRUDHandler(Deps{
		Perms:    perms,
		SQL:      sqlBuilder,
		Builder:  builder,
		Executor: mockgen.NewMockQueryExecutor(ctrl),
		DB:       mockDatabaseRegistry(ctrl),
		Roles:    roles,
		Reader:   reader,
	})
}

func selectPosts(h *CRUDHandler, schema, query string) *httptest.ResponseRecorder {
	req := crudRequest(http.MethodGet, "/prest-test/"+schema+"/posts"+query, map[string]string{
		"database": "prest-test", "schema": schema, "table": "posts",
	})
	rec := httptest.NewRecorder()
	h.Select(rec, req)
	return rec
}

func TestCRUDHandler_Select_readsAsTheConfiguredRole(t *testing.T) {
	t.Parallel()
	reader := &recordingReader{answer: answering(`[{"title":"a published post"}]`, nil)}
	h := anonymousRead(t, staticRoles{"prest-test": "app_anon"}, reader, "public")

	// Read a public table.
	// Expected: 200 with the rows, read once, as app_anon, from that table.
	rec := selectPosts(h, "public", "")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), "a published post")
	require.Equal(t, []recordedRead{{role: "app_anon", sql: `SELECT * FROM "public"."posts" `}}, reader.recorded())
}

func TestCRUDHandler_Select_countFirstReadsAsTheConfiguredRole(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	perms, sqlBuilder, _ := plainReadMocks(ctrl, "public", "posts")
	builder := mockgen.NewMockRequestQueryBuilder(ctrl)
	builder.EXPECT().DistinctClause(gomock.Any()).Return("", nil)
	builder.EXPECT().CountByRequest(gomock.Any()).Return(`SELECT COUNT(*) FROM`, nil)
	builder.EXPECT().JoinByRequest(gomock.Any()).Return(nil, nil)
	builder.EXPECT().WhereByRequest(gomock.Any(), 1).Return("", nil, nil)
	builder.EXPECT().GroupByClause(gomock.Any()).Return("")
	builder.EXPECT().TimeBucketClause(gomock.Any()).Return("", nil)
	builder.EXPECT().OrderByRequest(gomock.Any()).Return("", nil)
	builder.EXPECT().PaginateIfPossible(gomock.Any()).Return("", nil)
	reader := &recordingReader{answer: answering(`{"count":2}`, nil)}
	h := NewCRUDHandler(Deps{
		Perms: perms, SQL: sqlBuilder, Builder: builder,
		Executor: mockgen.NewMockQueryExecutor(ctrl),
		DB:       mockDatabaseRegistry(ctrl),
		Roles:    staticRoles{"prest-test": "app_anon"},
		Reader:   reader,
	})

	// Count a public table, count first.
	// Expected: 200 with the count, counted once, as app_anon.
	rec := selectPosts(h, "public", "?_count=*&_count_first=true")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, []recordedRead{{count: true, role: "app_anon", sql: `SELECT COUNT(*) FROM "public"."posts" `}}, reader.recorded())
}

func TestCRUDHandler_Select_refusesEverySchemaButPublicBeforeAnySQL(t *testing.T) {
	t.Parallel()
	for _, schema := range []string{"private", "pg_catalog", "information_schema", "Public", "public_", "auth"} {
		t.Run(schema, func(t *testing.T) {
			t.Parallel()
			reader := &recordingReader{answer: answering(`[{"title":"a row in `+schema+`"}]`, nil)}
			h := anonymousRead(t, staticRoles{"prest-test": "app_anon"}, reader, schema)

			// Read a table in a schema that is not public.
			// Expected: 404, no row in the body, and no read asked of anything.
			rec := selectPosts(h, schema, "")
			require.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
			require.NotContains(t, rec.Body.String(), "a row in")
			require.Empty(t, reader.recorded())
		})
	}
}

func TestCRUDHandler_Select_refusesADatabaseWithNoRoleBeforeAnySQL(t *testing.T) {
	t.Parallel()
	for name, roles := range map[string]adapters.AnonymousRoles{
		"no role for this database": staticRoles{"another-db": "app_anon"},
		"an empty role":             staticRoles{"prest-test": ""},
		"no role configuration":     nil,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			reader := &recordingReader{answer: answering(`[{"title":"a row the login could read"}]`, nil)}
			h := anonymousRead(t, roles, reader, "public")

			// Read a public table of a database given with no role.
			// Expected: 500, no row, and no read asked of anything.
			rec := selectPosts(h, "public", "")
			require.Equal(t, http.StatusInternalServerError, rec.Code, rec.Body.String())
			require.Contains(t, rec.Body.String(), "no anonymous role")
			require.NotContains(t, rec.Body.String(), "a row the login could read")
			require.Empty(t, reader.recorded())
		})
	}
}

func TestCRUDHandler_Select_refusesWithNoRoleReader(t *testing.T) {
	t.Parallel()
	h := anonymousRead(t, staticRoles{"prest-test": "app_anon"}, nil, "public")

	// Read a public table on an adapter that cannot read as a role.
	// Expected: 500, and the upstream executor, which reads as the login, is
	// never asked (its mock has no expectations).
	rec := selectPosts(h, "public", "")
	require.Equal(t, http.StatusInternalServerError, rec.Code, rec.Body.String())
}

func TestCRUDHandler_Select_servesNothingWhenTheRoleCannotBeEntered(t *testing.T) {
	t.Parallel()
	reader := &recordingReader{answer: &scanner.PrestScanner{
		Error: fmt.Errorf("%w: app_anon", adapters.ErrRoleNotEntered),
		Buff:  bytes.NewBufferString(`[{"title":"a row that must not arrive"}]`),
	}}
	h := anonymousRead(t, staticRoles{"prest-test": "app_anon"}, reader, "public")

	// Read a public table when SET LOCAL ROLE fails.
	// Expected: 500, and nothing the scanner held reaches the body.
	rec := selectPosts(h, "public", "")
	require.Equal(t, http.StatusInternalServerError, rec.Code, rec.Body.String())
	require.NotContains(t, rec.Body.String(), "must not arrive")
	require.Len(t, reader.recorded(), 1)
}

func TestCRUDHandler_Select_answersTheDatabasesRefusalAsForbidden(t *testing.T) {
	t.Parallel()
	reader := &recordingReader{answer: answering("", &pq.Error{
		Code:    "42501",
		Message: "permission denied for table posts",
	})}
	h := anonymousRead(t, staticRoles{"prest-test": "app_anon"}, reader, "public")

	// Read a public table the role holds no grant on.
	// Expected: 403, saying the database refused it.
	rec := selectPosts(h, "public", "")
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), "permission denied for table posts")
}
