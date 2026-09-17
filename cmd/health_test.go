package cmd

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/prest/prest/v2/config"
)

func TestProbeHealth(t *testing.T) {
	status := http.StatusOK
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/_health" {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(status)
	}))
	defer server.Close()

	// A server answering 200 at /_health.
	// Expected: healthy.
	require.NoError(t, probeHealth(server.URL+"/_health", time.Second))

	// The same server answering 503.
	// Expected: not healthy, naming the status.
	status = http.StatusServiceUnavailable
	err := probeHealth(server.URL+"/_health", time.Second)
	require.ErrorContains(t, err, "503")

	// A path the server does not have.
	// Expected: not healthy.
	status = http.StatusOK
	require.Error(t, probeHealth(server.URL+"/elsewhere", time.Second))

	// Nothing listening.
	// Expected: not healthy.
	server.Close()
	require.Error(t, probeHealth(server.URL+"/_health", time.Second))
}

func TestHealthURL(t *testing.T) {
	require.Equal(t, "http://127.0.0.1:3000/_health", healthURL(&config.Prest{HTTPPort: 3000, ContextPath: "/"}))
	require.Equal(t, "http://127.0.0.1:8080/api/_health", healthURL(&config.Prest{HTTPPort: 8080, ContextPath: "/api/"}))
	require.Equal(t, "https://127.0.0.1:443/_health", healthURL(&config.Prest{HTTPPort: 443, HTTPSMode: true}))
}
