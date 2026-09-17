package router

import (
	"net/http"

	"github.com/prest/prest/v2/config"
	"github.com/prest/prest/v2/controllers"
	"github.com/prest/prest/v2/middlewares"
	"github.com/prest/prest/v2/plugins"

	"github.com/gorilla/mux"
	"github.com/urfave/negroni/v3"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	"go.opentelemetry.io/otel/trace"
)

// RegisterRoutes wires HTTP routes onto the given router.
//
// miniship: rest answers two routes, the table read and the health address.
// pREST v2.4.2 registers 26 here: the database, schema and table listings, the
// stored-query runner and its registry, Go plugins, the sign-in route, the MCP
// door, Studio, readiness, the table description, the batch insert and every
// write verb. rest does not register them at all, rather than guarding them,
// because it is the one door of miniship the internet reaches directly.
// router/surface_test.go walks the router and fails on a third route. The
// handlers behind the removed routes stay in the tree, so an upstream rebase
// does not conflict on them, and the parameters that fed them stay in this
// signature, so the call sites upstream owns do not change.
func RegisterRoutes(
	router *mux.Router,
	cfg *config.Prest,
	h *controllers.Handlers,
	crudStack *middlewares.CRUDStack,
	_ *middlewares.QueryStack,
	_ *middlewares.AdminQueryStack,
	_ *plugins.Plugins,
) {
	// When telemetry is enabled, tag each request span with its matched route
	// template (http.route) so span/metric labels stay bounded to templates
	// instead of raw URLs. The middleware runs after mux matches the route.
	if cfg.Otel.Enabled {
		router.Use(otelRouteTagMiddleware)
	}

	// A write verb on the table path matches the path but not the method, and
	// gorilla/mux answers that 405 with an Allow header: it tells a caller the
	// path is served. rest answers it 404, as it answers every route it lacks.
	router.MethodNotAllowedHandler = http.NotFoundHandler()

	router.HandleFunc("/_health", h.Health.Handler()).Methods("GET")
	router.Handle("/{database}/{schema}/{table}", crudRoute(crudStack, h.CRUD.Select)).Methods("GET")
}

// otelRouteTagMiddleware annotates the active OpenTelemetry span with the
// matched gorilla/mux path template (http.route). It is a no-op for unmatched
// routes and when no recording span is active.
func otelRouteTagMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if route := mux.CurrentRoute(r); route != nil {
			if tmpl, err := route.GetPathTemplate(); err == nil {
				routeAttr := semconv.HTTPRoute(tmpl)
				span := trace.SpanFromContext(r.Context())
				// Semconv HTTP server span name: "{method} {route}".
				span.SetName(r.Method + " " + tmpl)
				span.SetAttributes(routeAttr)
				// Bind the route template to the otelhttp server metrics too.
				if labeler, ok := otelhttp.LabelerFromContext(r.Context()); ok {
					labeler.Add(routeAttr)
				}
			}
		}
		next.ServeHTTP(w, r)
	})
}

func crudRoute(stack *middlewares.CRUDStack, handler http.HandlerFunc) http.Handler {
	return negroni.New(append(stack.Handlers(), negroni.Wrap(handler))...)
}
