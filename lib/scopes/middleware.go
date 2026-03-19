package scopes

import (
	"fmt"
	"net/http"

	"github.com/go-chi/chi/v5"
)

// Middleware returns a chi middleware that enforces scoped permissions.
// It looks up the required scope for the matched route pattern and
// rejects requests that lack the required scope with 403 Forbidden.
//
// Routes not in the scope map are allowed through (e.g. health check
// or unauthenticated routes that shouldn't reach this middleware).
func Middleware() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Get the chi route pattern for this request
			rctx := chi.RouteContext(r.Context())
			if rctx == nil {
				next.ServeHTTP(w, r)
				return
			}

			pattern := rctx.RoutePattern()
			if pattern == "" {
				next.ServeHTTP(w, r)
				return
			}

			required, ok := ScopeForRoute(r.Method, pattern)
			if !ok {
				// Route not mapped — allow through
				next.ServeHTTP(w, r)
				return
			}

			if !HasScope(r.Context(), required) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusForbidden)
				fmt.Fprintf(w, `{"code":"Forbidden","message":"missing required scope: %s"}`, required)
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}
