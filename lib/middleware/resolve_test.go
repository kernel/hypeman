package middleware

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mockResolver records what name was passed to Resolve
type mockResolver struct {
	receivedName string
	returnErr    error
}

func (m *mockResolver) Resolve(ctx context.Context, idOrName string) (string, any, error) {
	m.receivedName = idOrName
	if m.returnErr != nil {
		return "", nil, m.returnErr
	}
	return idOrName, struct{}{}, nil
}

func TestResolveResource_URLDecodesImageName(t *testing.T) {
	// This test reproduces the bug where URL-encoded image names are not
	// properly decoded before being passed to the resolver.
	//
	// Bug: curl "https://server/images/docker.io%2Flibrary%2Fnginx:alpine"
	// The resolver receives "docker.io%2Flibrary%2Fnginx:alpine" (still encoded)
	// instead of "docker.io/library/nginx:alpine" (decoded).

	tests := []struct {
		name         string
		path         string
		expectedName string
	}{
		{
			name:         "simple image name",
			path:         "/images/alpine:latest",
			expectedName: "alpine:latest",
		},
		{
			name:         "URL-encoded slashes must be decoded",
			path:         "/images/docker.io%2Flibrary%2Fnginx%3Aalpine",
			expectedName: "docker.io/library/nginx:alpine", // Must be decoded!
		},
		{
			name:         "URL-encoded with colon",
			path:         "/images/docker.io%2Flibrary%2Falpine%3Alatest",
			expectedName: "docker.io/library/alpine:latest",
		},
		{
			name:         "nested repo URL-encoded",
			path:         "/images/docker.io%2Fonkernel%2Fchromium-headful%3Alatest",
			expectedName: "docker.io/onkernel/chromium-headful:latest",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resolver := &mockResolver{}

			errResponder := func(w http.ResponseWriter, err error, lookup string) {
				w.WriteHeader(http.StatusBadRequest)
				w.Write([]byte(`{"error":"` + err.Error() + `"}`))
			}

			middleware := ResolveResource(Resolvers{
				Image: resolver,
			}, errResponder)

			// Create a chi router to properly parse URL params
			r := chi.NewRouter()
			r.With(middleware).Get("/images/{name}", func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusOK)
			})

			req := httptest.NewRequest(http.MethodGet, tt.path, nil)
			w := httptest.NewRecorder()

			r.ServeHTTP(w, req)

			require.Equal(t, http.StatusOK, w.Code,
				"Expected 200 OK, got %d. Response: %s", w.Code, w.Body.String())

			assert.Equal(t, tt.expectedName, resolver.receivedName,
				"Resolver received wrong name - URL decoding may have failed")
		})
	}
}

func TestResolveResource_ResolvesBuilderByID(t *testing.T) {
	// Regression test: the path-dispatch switch must include a /builders/
	// case, otherwise the Builder resolver is never invoked and resolved
	// builders are missing from request context (handlers then 500).

	resolver := &mockResolver{}

	errResponder := func(w http.ResponseWriter, err error, lookup string) {
		w.WriteHeader(http.StatusNotFound)
	}

	middleware := ResolveResource(Resolvers{
		Builder: resolver,
	}, errResponder)

	r := chi.NewRouter()
	r.With(middleware).Get("/builders/{id}", func(w http.ResponseWriter, r *http.Request) {
		if GetResolvedBuilder[struct{}](r.Context()) == nil {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodGet, "/builders/bld_123", nil)
	w := httptest.NewRecorder()

	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code,
		"Expected resolved builder in context, got %d", w.Code)
	assert.Equal(t, "bld_123", resolver.receivedName,
		"Builder resolver was not invoked with the path ID")
}
