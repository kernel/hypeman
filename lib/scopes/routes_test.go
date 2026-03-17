package scopes_test

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/kernel/hypeman/lib/oapi"
	"github.com/kernel/hypeman/lib/scopes"
	"github.com/stretchr/testify/assert"
)

// TestAllRoutesHaveScopes builds a chi router identical to the production
// server and verifies that every registered route has either a scope mapping
// in RouteScopes or is explicitly listed in PublicRoutes. If a new endpoint
// is added without a scope mapping, this test fails.
func TestAllRoutesHaveScopes(t *testing.T) {
	r := chi.NewRouter()
	noop := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})

	// WebSocket endpoints registered outside OpenAPI (same as cmd/api/main.go)
	r.Get("/instances/{id}/exec", noop)
	r.Get("/instances/{id}/cp", noop)

	// Public/unauthenticated endpoints
	r.Get("/spec.yaml", noop)
	r.Get("/spec.json", noop)
	r.Get("/swagger", noop)

	// Registry endpoints — scoped by registry token auth, not API key scopes
	r.Route("/v2", func(r chi.Router) {
		r.Get("/token", noop)
		r.Handle("/*", noop)
	})

	// OpenAPI-generated routes (the bulk of the API)
	oapi.HandlerWithOptions(nil, oapi.ChiServerOptions{
		BaseRouter: r,
	})

	// Collect all routes and check them
	var missing []string
	err := chi.Walk(r, func(method, route string, handler http.Handler, middlewares ...func(http.Handler) http.Handler) error {
		// Normalize: chi.Walk returns patterns like "/instances/{id}/exec"
		// Strip trailing slashes for consistency
		route = strings.TrimRight(route, "/")
		if route == "" {
			route = "/"
		}

		key := method + " " + route

		// Skip registry routes — they use a separate token auth system
		if strings.HasPrefix(route, "/v2") {
			return nil
		}

		// Check if the route has a scope mapping or is explicitly public
		if _, hasScope := scopes.RouteScopes[key]; hasScope {
			return nil
		}
		if scopes.PublicRoutes[key] {
			return nil
		}

		missing = append(missing, key)
		return nil
	})
	assert.NoError(t, err)

	for _, route := range missing {
		t.Errorf("route %s has no scope mapping — add it to RouteScopes in lib/scopes/scopes.go (or PublicRoutes if intentionally unscoped)", route)
	}
}

// TestRouteScopesHaveNoStaleEntries verifies that every entry in RouteScopes
// corresponds to a route that actually exists in the router. This catches
// stale entries left behind when endpoints are removed.
func TestRouteScopesHaveNoStaleEntries(t *testing.T) {
	r := chi.NewRouter()
	noop := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})

	// Mirror production routes
	r.Get("/instances/{id}/exec", noop)
	r.Get("/instances/{id}/cp", noop)
	r.Get("/spec.yaml", noop)
	r.Get("/spec.json", noop)
	r.Get("/swagger", noop)

	oapi.HandlerWithOptions(nil, oapi.ChiServerOptions{
		BaseRouter: r,
	})

	// Collect all registered routes
	registered := make(map[string]bool)
	err := chi.Walk(r, func(method, route string, handler http.Handler, middlewares ...func(http.Handler) http.Handler) error {
		route = strings.TrimRight(route, "/")
		if route == "" {
			route = "/"
		}
		registered[method+" "+route] = true
		return nil
	})
	assert.NoError(t, err)

	for key := range scopes.RouteScopes {
		if !registered[key] {
			t.Errorf("RouteScopes contains %q but no such route exists — remove the stale entry from lib/scopes/scopes.go", key)
		}
	}
}

// TestPublicRoutesAreNotInRouteScopes ensures that routes marked as public
// don't also have a scope mapping, which would be contradictory.
func TestPublicRoutesAreNotInRouteScopes(t *testing.T) {
	for key := range scopes.PublicRoutes {
		_, hasScope := scopes.RouteScopes[key]
		assert.False(t, hasScope, fmt.Sprintf("route %s is in both PublicRoutes and RouteScopes — pick one", key))
	}
}
