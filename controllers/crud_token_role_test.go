package controllers

// miniship (miniship-cloud#801): the role a verified token named is the role
// the read becomes. middlewares/token_role.go is what verifies the token; what
// is driven here is that the table read takes the role it was handed and
// nothing else.

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	pctx "github.com/prest/prest/v2/context"
)

func selectPostsAs(h *CRUDHandler, role string) *httptest.ResponseRecorder {
	req := crudRequest(http.MethodGet, "/prest-test/public/posts", map[string]string{
		"database": "prest-test", "schema": "public", "table": "posts",
	})
	req = req.WithContext(pctx.WithTokenRole(req.Context(), role))
	rec := httptest.NewRecorder()
	h.Select(rec, req)
	return rec
}

func TestCRUDHandler_Select_readsAsTheRoleAVerifiedTokenNamed(t *testing.T) {
	t.Parallel()
	reader := &recordingReader{answer: answering(`[{"title":"every post"}]`, nil)}
	h := anonymousRead(t, staticRoles{"prest-test": "app_anon"}, reader, "public")

	// Read a public table as the role a verified token named.
	// Expected: one read, as service_role, and not as app_anon.
	rec := selectPostsAs(h, "service_role")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, []recordedRead{{role: "service_role", sql: `SELECT * FROM "public"."posts" `}}, reader.recorded())
}

func TestCRUDHandler_Select_readsAsTheAnonymousRoleWithNoToken(t *testing.T) {
	t.Parallel()
	reader := &recordingReader{answer: answering(`[{"title":"a published post"}]`, nil)}
	h := anonymousRead(t, staticRoles{"prest-test": "app_anon"}, reader, "public")

	// The same read with no token in the request.
	// Expected: as app_anon, the Database's anonymous role.
	rec := selectPosts(h, "public", "")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, []recordedRead{{role: "app_anon", sql: `SELECT * FROM "public"."posts" `}}, reader.recorded())
}

func TestCRUDHandler_Select_aTokensAnswerIsNeverCachedForAnAnonymousCaller(t *testing.T) {
	t.Parallel()
	reader := &recordingReader{answer: answering(`[{"title":"a post only service_role reads"}]`, nil)}
	h := anonymousRead(t, staticRoles{"prest-test": "app_anon"}, reader, "public")
	cache := &recordingCacher{}
	h.cache = cache

	// A service_role read, with the response cache on.
	// Expected: served, and not written under a key an anonymous caller's
	// identical request would be answered from.
	rec := selectPostsAs(h, "service_role")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Empty(t, cache.value, "a service_role answer was cached")
}
