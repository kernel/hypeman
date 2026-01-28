package registry

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/golang-jwt/jwt/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestBuildKitAuthFlow simulates BuildKit's authentication flow with the registry.
// This test reproduces the issue where BuildKit makes anonymous token requests
// and expects a proper WWW-Authenticate challenge to retry with credentials.
func TestBuildKitAuthFlow(t *testing.T) {
	jwtSecret := "test-secret-key"
	tokenHandler := NewTokenHandler(jwtSecret)

	// Create a router that mimics the hypeman /v2 endpoint structure
	r := chi.NewRouter()
	r.Route("/v2", func(r chi.Router) {
		r.Get("/token", tokenHandler.ServeHTTP)
		// Mock registry endpoint that returns 401 with Bearer challenge
		r.Get("/*", func(w http.ResponseWriter, r *http.Request) {
			// Simulate registry returning 401 with WWW-Authenticate: Bearer
			w.Header().Set("WWW-Authenticate", `Bearer realm="http://localhost/v2/token",service="hypeman"`)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`{"errors":[{"code":"UNAUTHORIZED","message":"authentication required"}]}`))
		})
	})

	server := httptest.NewServer(r)
	defer server.Close()

	// Generate a valid registry token (like what builder VM would have)
	registryToken := generateTestToken(t, jwtSecret, "build-123", []string{"builds/build-123", "cache/org-test"}, "push")

	t.Run("anonymous token request for cache import returns auth challenge", func(t *testing.T) {
		// This simulates BuildKit's first request to token endpoint (anonymous)
		// when trying to import cache
		resp, err := http.Get(server.URL + "/v2/token?scope=repository:cache/org-test:pull&service=hypeman")
		require.NoError(t, err)
		defer resp.Body.Close()

		// Should get 401 with WWW-Authenticate header
		assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)

		wwwAuth := resp.Header.Get("WWW-Authenticate")
		assert.NotEmpty(t, wwwAuth, "WWW-Authenticate header must be present")
		assert.Contains(t, wwwAuth, "Basic", "should challenge with Basic auth")
		assert.Contains(t, wwwAuth, "realm=", "should include realm")
	})

	t.Run("authenticated token request for cache import succeeds", func(t *testing.T) {
		// This simulates BuildKit retrying with credentials
		req, err := http.NewRequest(http.MethodGet, server.URL+"/v2/token?scope=repository:cache/org-test:pull&service=hypeman", nil)
		require.NoError(t, err)

		// Add Basic auth with JWT as username (how BuildKit sends credentials)
		basicAuth := base64.StdEncoding.EncodeToString([]byte(registryToken + ":"))
		req.Header.Set("Authorization", "Basic "+basicAuth)

		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		defer resp.Body.Close()

		assert.Equal(t, http.StatusOK, resp.StatusCode)

		var tokenResp TokenResponse
		err = json.NewDecoder(resp.Body).Decode(&tokenResp)
		require.NoError(t, err)
		assert.NotEmpty(t, tokenResp.Token)
	})

	t.Run("anonymous token request for image push returns auth challenge", func(t *testing.T) {
		// This simulates BuildKit's first request when pushing an image
		resp, err := http.Get(server.URL + "/v2/token?scope=repository:builds/build-123:push,pull&service=hypeman")
		require.NoError(t, err)
		defer resp.Body.Close()

		assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)

		wwwAuth := resp.Header.Get("WWW-Authenticate")
		assert.NotEmpty(t, wwwAuth, "WWW-Authenticate header must be present")
		assert.Contains(t, wwwAuth, "Basic")
	})

	t.Run("authenticated token request for image push succeeds", func(t *testing.T) {
		req, err := http.NewRequest(http.MethodGet, server.URL+"/v2/token?scope=repository:builds/build-123:push,pull&service=hypeman", nil)
		require.NoError(t, err)

		basicAuth := base64.StdEncoding.EncodeToString([]byte(registryToken + ":"))
		req.Header.Set("Authorization", "Basic "+basicAuth)

		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		defer resp.Body.Close()

		assert.Equal(t, http.StatusOK, resp.StatusCode)
	})

	t.Run("authenticated request for unauthorized repo returns 403", func(t *testing.T) {
		// Token only allows access to builds/build-123 and cache/org-test
		// Request for a different repo should fail with 403
		req, err := http.NewRequest(http.MethodGet, server.URL+"/v2/token?scope=repository:builds/other-build:push&service=hypeman", nil)
		require.NoError(t, err)

		basicAuth := base64.StdEncoding.EncodeToString([]byte(registryToken + ":"))
		req.Header.Set("Authorization", "Basic "+basicAuth)

		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		defer resp.Body.Close()

		assert.Equal(t, http.StatusForbidden, resp.StatusCode)
	})
}

// TestDockerConfigCredentialLookup tests that credentials stored in docker config.json
// format work correctly with the token endpoint.
func TestDockerConfigCredentialLookup(t *testing.T) {
	jwtSecret := "test-secret"
	tokenHandler := NewTokenHandler(jwtSecret)

	// Simulate docker config.json auth format: base64(username:password)
	// For our use case: base64(jwt_token:)
	registryToken := generateTestToken(t, jwtSecret, "build-456", []string{"builds/build-456", "cache/tenant-x"}, "push")

	// This is exactly how docker config.json stores the auth value
	dockerConfigAuth := base64.StdEncoding.EncodeToString([]byte(registryToken + ":"))

	tests := []struct {
		name           string
		scope          string
		expectedStatus int
	}{
		{
			name:           "cache pull with valid credentials",
			scope:          "repository:cache/tenant-x:pull",
			expectedStatus: http.StatusOK,
		},
		{
			name:           "cache push with valid credentials",
			scope:          "repository:cache/tenant-x:push",
			expectedStatus: http.StatusOK,
		},
		{
			name:           "image push with valid credentials",
			scope:          "repository:builds/build-456:push,pull",
			expectedStatus: http.StatusOK,
		},
		{
			name:           "unauthorized repo",
			scope:          "repository:builds/other:push",
			expectedStatus: http.StatusForbidden,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/v2/token?scope="+tt.scope+"&service=hypeman", nil)
			req.Header.Set("Authorization", "Basic "+dockerConfigAuth)

			rr := httptest.NewRecorder()
			tokenHandler.ServeHTTP(rr, req)

			assert.Equal(t, tt.expectedStatus, rr.Code)
		})
	}
}

// generateTestToken creates a JWT token for testing
func generateTestToken(t *testing.T, secret, buildID string, repos []string, scope string) string {
	t.Helper()
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"sub":      "builder-" + buildID,
		"iat":      time.Now().Unix(),
		"exp":      time.Now().Add(time.Hour).Unix(),
		"iss":      "hypeman",
		"build_id": buildID,
		"repos":    repos,
		"scope":    scope,
	})
	tokenString, err := token.SignedString([]byte(secret))
	require.NoError(t, err)
	return tokenString
}
