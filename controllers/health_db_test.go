package controllers

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

type countingPinger struct {
	pings    int
	pingAlls int
	err      error
}

func (p *countingPinger) Ping(context.Context) error    { p.pings++; return p.err }
func (p *countingPinger) PingAll(context.Context) error { p.pingAlls++; return p.err }

func TestDefaultCheckList_healthSkipsDatabase(t *testing.T) {
	t.Parallel()

	p := &countingPinger{err: errors.New("database is down")}
	h := NewHealthHandler(DefaultCheckList(p))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/_health", nil))

	require.Equal(t, http.StatusOK, rec.Code)
	require.Zero(t, p.pings)
	require.Zero(t, p.pingAlls)
}

func TestDefaultReadyCheckList_readyChecksDatabases(t *testing.T) {
	t.Parallel()

	p := &countingPinger{err: errors.New("database is down")}
	h := NewHealthHandler(DefaultReadyCheckList(p))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/_ready", nil))

	require.Equal(t, http.StatusServiceUnavailable, rec.Code)
	require.Equal(t, 1, p.pingAlls)
	require.Zero(t, p.pings)
}

func TestCheckDBHealth_pingsDatabase(t *testing.T) {
	t.Parallel()

	p := &countingPinger{}
	require.NoError(t, CheckDBHealth(p)(context.Background()))
	require.Equal(t, 1, p.pings)

	down := &countingPinger{err: errors.New("database is down")}
	require.Error(t, CheckDBHealth(down)(context.Background()))
}
