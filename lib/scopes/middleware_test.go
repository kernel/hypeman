package scopes_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/kernel/hypeman/lib/scopes"
	"github.com/stretchr/testify/assert"
)

// TestMiddleware_EnforcesScopes proves that the scope middleware actually
// blocks requests when the token lacks the required scope. This is an
// integration test using a real chi router to verify RoutePattern() is
// available when the middleware runs.
func TestMiddleware_EnforcesScopes(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	})

	buildRouter := func() chi.Router {
		r := chi.NewRouter()
		r.Group(func(r chi.Router) {
			r.Use(scopes.Middleware())
			// Register routes matching RouteScopes entries
			r.Get("/instances", handler)
			r.Post("/instances", handler)
			r.Delete("/instances/{id}", handler)
			r.Get("/instances/{id}", handler)
			r.Get("/images", handler)
			r.Post("/images", handler)
		})
		return r
	}

	t.Run("allows request when scope matches", func(t *testing.T) {
		r := buildRouter()
		ctx := scopes.ContextWithPermissions(context.Background(), []scopes.Scope{scopes.InstanceRead})
		req := httptest.NewRequest(http.MethodGet, "/instances", nil).WithContext(ctx)
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)
		assert.Equal(t, http.StatusOK, rr.Code)
	})

	t.Run("blocks request when scope is missing", func(t *testing.T) {
		r := buildRouter()
		// Token has image:read but not instance:read
		ctx := scopes.ContextWithPermissions(context.Background(), []scopes.Scope{scopes.ImageRead})
		req := httptest.NewRequest(http.MethodGet, "/instances", nil).WithContext(ctx)
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)
		assert.Equal(t, http.StatusForbidden, rr.Code)
		assert.Contains(t, rr.Body.String(), "instance:read")
	})

	t.Run("blocks write when only read scope present", func(t *testing.T) {
		r := buildRouter()
		ctx := scopes.ContextWithPermissions(context.Background(), []scopes.Scope{scopes.InstanceRead})
		req := httptest.NewRequest(http.MethodPost, "/instances", nil).WithContext(ctx)
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)
		assert.Equal(t, http.StatusForbidden, rr.Code)
		assert.Contains(t, rr.Body.String(), "instance:write")
	})

	t.Run("blocks delete when only read and write scopes present", func(t *testing.T) {
		r := buildRouter()
		ctx := scopes.ContextWithPermissions(context.Background(), []scopes.Scope{scopes.InstanceRead, scopes.InstanceWrite})
		req := httptest.NewRequest(http.MethodDelete, "/instances/abc", nil).WithContext(ctx)
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)
		assert.Equal(t, http.StatusForbidden, rr.Code)
		assert.Contains(t, rr.Body.String(), "instance:delete")
	})

	t.Run("wildcard allows everything", func(t *testing.T) {
		r := buildRouter()
		ctx := scopes.ContextWithPermissions(context.Background(), []scopes.Scope{scopes.All})
		for _, tc := range []struct {
			method string
			path   string
		}{
			{"GET", "/instances"},
			{"POST", "/instances"},
			{"DELETE", "/instances/abc"},
			{"GET", "/images"},
			{"POST", "/images"},
		} {
			req := httptest.NewRequest(tc.method, tc.path, nil).WithContext(ctx)
			rr := httptest.NewRecorder()
			r.ServeHTTP(rr, req)
			assert.Equal(t, http.StatusOK, rr.Code, "%s %s should be allowed with wildcard", tc.method, tc.path)
		}
	})

	t.Run("legacy token without permissions allows everything", func(t *testing.T) {
		r := buildRouter()
		// No ContextWithPermissions — simulates legacy token
		req := httptest.NewRequest(http.MethodDelete, "/instances/abc", nil)
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)
		assert.Equal(t, http.StatusOK, rr.Code)
	})

	t.Run("empty permissions denies everything", func(t *testing.T) {
		r := buildRouter()
		ctx := scopes.ContextWithPermissions(context.Background(), []scopes.Scope{})
		req := httptest.NewRequest(http.MethodGet, "/instances", nil).WithContext(ctx)
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)
		assert.Equal(t, http.StatusForbidden, rr.Code)
	})
}
