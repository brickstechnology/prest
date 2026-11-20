package controllers_test

import (
	"net/http"
	"testing"

	"github.com/prest/prest/v2/integration/helpers"
	"github.com/prest/prest/v2/integration/testutils"
)

// /_health is a liveness probe: it answers from the process alone and never
// depends on the database. /_ready is the readiness probe that pings the
// databases (see TestReadyEndpoint and the samehost suite for the 503 case).
func TestHealthIsLiveness(t *testing.T) {
	base := helpers.ServerURL(t)

	for _, tc := range []struct {
		desc     string
		path     string
		expected int
		body     string
	}{
		{"liveness answers OK and names the postgres adapter", "/_health", http.StatusOK, `"adapter":"postgres"`},
		{"readiness answers OK when the database is reachable", "/_ready", http.StatusOK, ""},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			// What: GET the probe endpoint on the default prestd.
			// Expected outcome: HTTP 200, plus the adapter name for /_health.
			// Why: /_health is liveness and /_ready is readiness; on a healthy
			// stack both succeed.
			if tc.body == "" {
				testutils.DoRequest(t, base+tc.path, nil, "GET", tc.expected, tc.desc)
				return
			}
			testutils.DoRequest(t, base+tc.path, nil, "GET", tc.expected, tc.desc, tc.body)
		})
	}
}
