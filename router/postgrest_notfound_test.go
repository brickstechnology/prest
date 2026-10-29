package router

// miniship-cloud#1888: a route rest lacks is a plain 404 everywhere but on the
// SDK's address, where it is PostgREST 14's PGRST125.

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNotFound_onTheSDKAddressIsPGRST125(t *testing.T) {
	for _, path := range []string{"/rest/v1/a/b", "/rest/v1/rpc/f/g"} {
		rec := httptest.NewRecorder()
		notFound(rec, httptest.NewRequest(http.MethodGet, path, nil))
		require.Equal(t, http.StatusNotFound, rec.Code, path)
		require.JSONEq(t, `{"code":"PGRST125","details":null,"hint":null,"message":"Invalid path specified in request URL"}`, rec.Body.String(), path)
		require.Equal(t, "application/json; charset=utf-8", rec.Header().Get("Content-Type"))
		require.Equal(t, "PostgREST; error=PGRST125", rec.Header().Get("Proxy-Status"))
	}
}

func TestNotFound_elsewhereIsThePlainOne(t *testing.T) {
	rec := httptest.NewRecorder()
	notFound(rec, httptest.NewRequest(http.MethodGet, "/prest/public/anything", nil))
	require.Equal(t, http.StatusNotFound, rec.Code)
	require.Contains(t, rec.Body.String(), "404 page not found")
	require.Empty(t, rec.Header().Get("Proxy-Status"))
}
