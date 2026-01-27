package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/go-chi/chi/v5"
	"github.com/golang-jwt/jwt/v5"
	mw "github.com/kernel/hypeman/lib/middleware"
	"github.com/kernel/hypeman/lib/oapi"
	nethttpmiddleware "github.com/oapi-codegen/nethttp-middleware"
	"github.com/oapi-codegen/runtime"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testJWTSecret = "test-secret-key"

func generateValidJWT(userID string) (string, error) {
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"sub": userID,
		"exp": time.Now().Add(time.Hour).Unix(),
	})
	return token.SignedString([]byte(testJWTSecret))
}

func setupTestRouter(t *testing.T) http.Handler {
	spec, err := oapi.GetSwagger()
	require.NoError(t, err)
	spec.Servers = nil

	r := chi.NewRouter()
	r.Use(nethttpmiddleware.OapiRequestValidatorWithOptions(spec, &nethttpmiddleware.Options{
		Options: openapi3filter.Options{
			AuthenticationFunc: mw.OapiAuthenticationFunc(testJWTSecret),
		},
		ErrorHandler: mw.OapiErrorHandler,
	}))

	// Simple handler for testing
	r.Post("/images", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte(`{"id":"test"}`))
	})

	return r
}

func TestMiddleware_InvalidPayload(t *testing.T) {
	router := setupTestRouter(t)
	token, err := generateValidJWT("user-123")
	require.NoError(t, err)

	// Missing required "name" field
	req := httptest.NewRequest(http.MethodPost, "/images", bytes.NewBufferString(`{}`))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")

	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestMiddleware_InvalidJWT(t *testing.T) {
	router := setupTestRouter(t)

	req := httptest.NewRequest(http.MethodPost, "/images", bytes.NewBufferString(`{"name":"test"}`))
	req.Header.Set("Authorization", "Bearer invalid-token")
	req.Header.Set("Content-Type", "application/json")

	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestMiddleware_ValidJWT(t *testing.T) {
	router := setupTestRouter(t)
	token, err := generateValidJWT("user-123")
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodPost, "/images", bytes.NewBufferString(`{"name":"docker.io/library/nginx:latest"}`))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")

	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusCreated, w.Code)
}

func TestOapiRuntimeBindStyledParameter_URLDecoding(t *testing.T) {
	// Test if oapi-codegen's runtime.BindStyledParameterWithOptions URL-decodes path params
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "simple string",
			input:    "alpine:latest",
			expected: "alpine:latest",
		},
		{
			name:     "URL-encoded slashes",
			input:    "docker.io%2Flibrary%2Falpine%3Alatest",
			expected: "docker.io/library/alpine:latest", // Should be decoded
		},
		{
			name:     "already decoded",
			input:    "docker.io/library/alpine:latest",
			expected: "docker.io/library/alpine:latest",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var dest string
			err := runtime.BindStyledParameterWithOptions(
				"simple", "name", tt.input, &dest,
				runtime.BindStyledParameterOptions{
					ParamLocation: runtime.ParamLocationPath,
					Explode:       false,
					Required:      true,
				},
			)
			require.NoError(t, err)
			assert.Equal(t, tt.expected, dest, "BindStyledParameterWithOptions should URL-decode the input")
		})
	}
}

func TestImageNameWithSlashes_URLEncoding(t *testing.T) {
	// This test verifies how chi router handles image names with slashes.
	// Image names like "docker.io/onkernel/chromium-headful:latest" contain slashes
	// that need to be URL-encoded to work with the /images/{name} endpoint.
	//
	// FINDINGS:
	// 1. Chi DOES route to the handler when slashes are URL-encoded (%2F)
	// 2. Chi's URLParam returns the STILL-ENCODED value (e.g., "docker.io%2Flibrary%2Falpine%3Alatest")
	// 3. The handler/middleware must URL-decode the parameter itself
	// 4. The oapi-codegen runtime.BindStyledParameterWithOptions MAY handle decoding
	//
	// This is a server-side documentation issue - users need to know to URL-encode image names
	// with slashes, and the server needs to ensure proper URL-decoding.

	r := chi.NewRouter()

	var capturedRaw string
	var capturedDecoded string
	r.Get("/images/{name}", func(w http.ResponseWriter, req *http.Request) {
		capturedRaw = chi.URLParam(req, "name")
		// URL-decode the parameter
		decoded, _ := url.QueryUnescape(capturedRaw)
		capturedDecoded = decoded
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"name":"` + capturedDecoded + `"}`))
	})

	token, err := generateValidJWT("user-123")
	require.NoError(t, err)

	tests := []struct {
		name            string
		path            string
		expectedStatus  int
		expectedRaw     string
		expectedDecoded string
	}{
		{
			name:            "simple image name (no slashes)",
			path:            "/images/alpine:latest",
			expectedStatus:  http.StatusOK,
			expectedRaw:     "alpine:latest",
			expectedDecoded: "alpine:latest",
		},
		{
			name:            "URL-encoded slashes - routes correctly",
			path:            "/images/docker.io%2Flibrary%2Falpine%3Alatest",
			expectedStatus:  http.StatusOK,
			expectedRaw:     "docker.io%2Flibrary%2Falpine%3Alatest", // chi returns encoded
			expectedDecoded: "docker.io/library/alpine:latest",       // after QueryUnescape
		},
		{
			name:            "unencoded slashes - route not matched",
			path:            "/images/docker.io/library/alpine:latest",
			expectedStatus:  http.StatusNotFound, // chi won't match this route
			expectedRaw:     "",
			expectedDecoded: "",
		},
		{
			name:            "nested image docker.io/onkernel/chromium-headful:latest",
			path:            "/images/docker.io%2Fonkernel%2Fchromium-headful%3Alatest",
			expectedStatus:  http.StatusOK,
			expectedRaw:     "docker.io%2Fonkernel%2Fchromium-headful%3Alatest",
			expectedDecoded: "docker.io/onkernel/chromium-headful:latest",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			capturedRaw = ""
			capturedDecoded = ""

			req := httptest.NewRequest(http.MethodGet, tt.path, nil)
			req.Header.Set("Authorization", "Bearer "+token)

			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)

			assert.Equal(t, tt.expectedStatus, w.Code, "status code mismatch for path %s", tt.path)
			if tt.expectedStatus == http.StatusOK {
				assert.Equal(t, tt.expectedRaw, capturedRaw, "raw captured name mismatch")
				assert.Equal(t, tt.expectedDecoded, capturedDecoded, "decoded name mismatch")
			}
		})
	}
}
