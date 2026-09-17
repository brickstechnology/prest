package router_test

// rest's surface. pREST v2.4.2 registers 26 routes in RegisterRoutes; rest
// keeps the table read and the health address and nothing else, because it is
// the one door of miniship the internet reaches directly. app/surface_test.go
// sends a request of every removed shape through the composed stack.

import (
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/gorilla/mux"
	"github.com/stretchr/testify/require"

	"github.com/prest/prest/v2/adapters/mock"
	"github.com/prest/prest/v2/config"
	"github.com/prest/prest/v2/controllers"
	"github.com/prest/prest/v2/middlewares"
	"github.com/prest/prest/v2/plugins"
	"github.com/prest/prest/v2/router"
)

// keptRoutes is everything rest answers, as "METHODS TEMPLATE".
var keptRoutes = []string{
	"GET /_health",
	"GET /{database}/{schema}/{table}",
}

// everythingOn is a config under which pREST registers every optional route it
// has: sign-in, the stored-query registry, Studio and the telemetry tagger.
func everythingOn() *config.Prest {
	return &config.Prest{
		AuthEnabled: true,
		JWTKey:      "a-key-long-enough-for-hs256-signing",
		PGDatabase:  "given",
		StudioConf:  config.StudioConf{Enabled: true},
		Otel:        config.OtelConf{Enabled: true},
		QueriesConf: config.QueriesConf{
			RegisterEnabled: true,
			Storage:         config.QueriesStorageDatabase,
		},
	}
}

func registeredRoutes(t *testing.T, cfg *config.Prest) []string {
	t.Helper()
	cfg.Adapter = mock.New(t)
	h := controllers.NewHandlersFromConfig(cfg)
	h.QueryRegistry = controllers.NewQueryRegistryHandler(controllers.NewDepsFromConfig(cfg), cfg.QueriesConf)
	plg := plugins.New(cfg)

	r := mux.NewRouter().StrictSlash(true)
	router.RegisterRoutes(
		r, cfg, h,
		middlewares.NewCRUDStack(cfg, plg),
		middlewares.NewQueryStack(cfg, nil),
		middlewares.NewAdminQueryStack(cfg),
		plg,
	)

	var got []string
	err := r.Walk(func(route *mux.Route, _ *mux.Router, _ []*mux.Route) error {
		tmpl, err := route.GetPathTemplate()
		if err != nil {
			return fmt.Errorf("route without a path template: %w", err)
		}
		methods, err := route.GetMethods()
		if err != nil {
			methods = []string{"ANY"}
		}
		got = append(got, strings.Join(methods, ",")+" "+tmpl)
		return nil
	})
	require.NoError(t, err)
	sort.Strings(got)
	return got
}

// Upstream registers 26 routes under this config. A route upstream adds on a
// later rebase turns this red instead of opening.
func TestRegisterRoutes_keepsOnlyTheTableReadAndTheHealthAddress(t *testing.T) {
	require.Equal(t, keptRoutes, registeredRoutes(t, everythingOn()))
}

// Upstream registers 25 routes here, all but sign-in: Studio's prefix is
// registered with Studio off, and registeredRoutes always supplies the
// stored-query registry.
func TestRegisterRoutes_keepsTheSameTwoWithEverythingOff(t *testing.T) {
	require.Equal(t, keptRoutes, registeredRoutes(t, &config.Prest{PGDatabase: "given"}))
}
