package controllers

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/golang/mock/gomock"
	"github.com/prest/prest/v2/adapters/mock"
	"github.com/prest/prest/v2/adapters/mockgen"
	"github.com/prest/prest/v2/cache"
	"github.com/prest/prest/v2/config"
	"github.com/stretchr/testify/require"
)

func TestNewDepsFromConfig(t *testing.T) {
	t.Parallel()

	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	adapter := mockgen.NewMockAdapter(ctrl)
	p := &config.Prest{
		Adapter:      adapter,
		PGDatabase:   "prest-test",
		AuthEnabled:  true,
		AuthType:     "body",
		JWTKey:       "test-jwt-hmac-secret-key-32bytes",
		AuthSchema:   "public",
		AuthTable:    "users",
		AuthUsername: "username",
		AuthPassword: "password",
		AuthEncrypt:  "MD5",
		AuthTokenTTL: 30 * time.Minute,
		Cache:        cache.Config{Enabled: true},
		AccessConf:   config.AccessConf{AllowUnscopedWrites: true},
	}

	deps := NewDepsFromConfig(p)
	require.Equal(t, adapter, deps.Catalog)
	require.Equal(t, adapter, deps.DB)
	require.Equal(t, adapter, deps.Executor)
	require.Equal(t, "prest-test", deps.PGDatabase)
	require.NotNil(t, deps.Cache)
	require.True(t, deps.Auth.Enabled)
	require.Equal(t, "body", deps.Auth.AuthType)
	require.Equal(t, "test-jwt-hmac-secret-key-32bytes", deps.Auth.JWTKey)
	require.Equal(t, 30*time.Minute, deps.Auth.TokenTTL)
	require.True(t, deps.AllowUnscopedWrites)
}

func TestNewDepsFromConfig_CacheDisabled(t *testing.T) {
	t.Parallel()

	p := &config.Prest{Cache: cache.Config{Enabled: false}}
	deps := NewDepsFromConfig(p)
	require.Nil(t, deps.Cache)
}

func TestNewHandlers(t *testing.T) {
	t.Parallel()

	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	adapter := mockgen.NewMockAdapter(ctrl)
	h := NewHandlers(Deps{
		Catalog:  adapter,
		Builder:  adapter,
		Executor: adapter,
		SQL:      adapter,
		Perms:    adapter,
		Scripts:  adapter,
		DB:       adapter,
	}, nil)

	require.NotNil(t, h.Auth)
	require.NotNil(t, h.Catalog)
	require.NotNil(t, h.MCP)
	require.NotNil(t, h.Table)
	require.NotNil(t, h.CRUD)
	require.NotNil(t, h.Script)
	require.NotNil(t, h.Health)
}

// NewHandlers wires /_health to the liveness list and /_ready to the
// readiness list, so only /_ready reaches the pinger.
func TestNewHandlers_healthAndReadyWiring(t *testing.T) {
	t.Parallel()

	p := &countingPinger{err: errors.New("database is down")}
	h := NewHandlers(Deps{Pinger: p, Readiness: p}, nil)

	rec := httptest.NewRecorder()
	h.Health.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/_health", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	require.Zero(t, p.pings+p.pingAlls)

	rec = httptest.NewRecorder()
	h.Ready.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/_ready", nil))
	require.Equal(t, http.StatusServiceUnavailable, rec.Code)
	require.Equal(t, 1, p.pingAlls)
}

func TestNewHandlersFromConfig(t *testing.T) {
	t.Parallel()

	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	adapter := mockgen.NewMockAdapter(ctrl)
	h := NewHandlersFromConfig(&config.Prest{Adapter: adapter})
	require.NotNil(t, h.CRUD)
}

func TestNewHandlers_QueryRegistryRequiresAdapter(t *testing.T) {
	t.Parallel()

	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	adapter := mockgen.NewMockAdapter(ctrl)
	cfg := &config.Prest{
		QueriesConf: config.QueriesConf{
			RegisterEnabled: true,
			Storage:         config.QueriesStorageDatabase,
		},
	}

	h := NewHandlers(Deps{
		Catalog:  adapter,
		Builder:  adapter,
		Executor: adapter,
		SQL:      adapter,
		Perms:    adapter,
		Scripts:  adapter,
		DB:       adapter,
	}, cfg)
	require.Nil(t, h.QueryRegistry)
}

func TestNewHandlers_QueryRegistryWithAdapter(t *testing.T) {
	t.Parallel()

	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	registry := mockgen.NewMockQueryRegistry(ctrl)
	cfg := &config.Prest{
		QueriesConf: config.QueriesConf{
			RegisterEnabled: true,
			Storage:         config.QueriesStorageDatabase,
		},
	}

	h := NewHandlers(Deps{
		QueryRegistry: registry,
		DB:            mockgen.NewMockAdapter(ctrl),
	}, cfg)
	require.NotNil(t, h.QueryRegistry)
}

// Dialect is optional: an adapter without it leaves deps.Dialect nil so auth
// reports ErrAuthDialectMissing instead of panicking.
func TestNewDepsFromConfig_DialectOptional(t *testing.T) {
	t.Parallel()

	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	deps := NewDepsFromConfig(&config.Prest{Adapter: mockgen.NewMockAdapter(ctrl)})
	require.Nil(t, deps.Dialect)

	withDialect := mock.New(t)
	deps = NewDepsFromConfig(&config.Prest{Adapter: withDialect})
	require.Equal(t, withDialect, deps.Dialect)
}
