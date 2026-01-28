package registry

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testJWTSecret = "test-secret-key"

// generateRegistryToken creates a registry token with the given repos and scope
func generateRegistryToken(t *testing.T, buildID string, repos []string, scope string, ttl time.Duration) string {
	t.Helper()
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"sub":      "builder-" + buildID,
		"iat":      time.Now().Unix(),
		"exp":      time.Now().Add(ttl).Unix(),
		"iss":      "hypeman",
		"build_id": buildID,
		"repos":    repos,
		"scope":    scope,
	})
	tokenString, err := token.SignedString([]byte(testJWTSecret))
	require.NoError(t, err)
	return tokenString
}

func TestTokenHandler_BasicAuth(t *testing.T) {
	handler := NewTokenHandler(testJWTSecret)

	t.Run("valid basic auth returns token", func(t *testing.T) {
		registryToken := generateRegistryToken(t, "build-123", []string{"builds/build-123"}, "push", time.Hour)
		basicAuth := base64.StdEncoding.EncodeToString([]byte(registryToken + ":"))

		req := httptest.NewRequest(http.MethodGet, "/v2/token?scope=repository:builds/build-123:push", nil)
		req.Header.Set("Authorization", "Basic "+basicAuth)

		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)

		assert.Equal(t, http.StatusOK, rr.Code)

		var resp TokenResponse
		err := json.NewDecoder(rr.Body).Decode(&resp)
		require.NoError(t, err)
		assert.NotEmpty(t, resp.Token)
		assert.NotEmpty(t, resp.AccessToken)
	})

	t.Run("invalid basic auth returns 401", func(t *testing.T) {
		basicAuth := base64.StdEncoding.EncodeToString([]byte("invalid-token:"))

		req := httptest.NewRequest(http.MethodGet, "/v2/token?scope=repository:builds/build-123:push", nil)
		req.Header.Set("Authorization", "Basic "+basicAuth)

		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)

		assert.Equal(t, http.StatusUnauthorized, rr.Code)
	})
}

func TestTokenHandler_BearerAuth(t *testing.T) {
	handler := NewTokenHandler(testJWTSecret)

	t.Run("valid bearer auth returns token", func(t *testing.T) {
		registryToken := generateRegistryToken(t, "build-456", []string{"builds/build-456"}, "push", time.Hour)

		req := httptest.NewRequest(http.MethodGet, "/v2/token?scope=repository:builds/build-456:push", nil)
		req.Header.Set("Authorization", "Bearer "+registryToken)

		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)

		assert.Equal(t, http.StatusOK, rr.Code)

		var resp TokenResponse
		err := json.NewDecoder(rr.Body).Decode(&resp)
		require.NoError(t, err)
		assert.NotEmpty(t, resp.Token)
	})
}

func TestTokenHandler_ScopeValidation(t *testing.T) {
	handler := NewTokenHandler(testJWTSecret)

	t.Run("scope not in token is rejected", func(t *testing.T) {
		// Token allows builds/build-123, but request is for builds/other
		registryToken := generateRegistryToken(t, "build-123", []string{"builds/build-123"}, "push", time.Hour)
		basicAuth := base64.StdEncoding.EncodeToString([]byte(registryToken + ":"))

		req := httptest.NewRequest(http.MethodGet, "/v2/token?scope=repository:builds/other:push", nil)
		req.Header.Set("Authorization", "Basic "+basicAuth)

		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)

		assert.Equal(t, http.StatusForbidden, rr.Code)
	})

	t.Run("push action with pull-only token is rejected", func(t *testing.T) {
		// Token only has pull scope
		registryToken := generateRegistryToken(t, "build-123", []string{"builds/build-123"}, "pull", time.Hour)
		basicAuth := base64.StdEncoding.EncodeToString([]byte(registryToken + ":"))

		req := httptest.NewRequest(http.MethodGet, "/v2/token?scope=repository:builds/build-123:push", nil)
		req.Header.Set("Authorization", "Basic "+basicAuth)

		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)

		assert.Equal(t, http.StatusForbidden, rr.Code)
	})

	t.Run("pull action with push token is allowed", func(t *testing.T) {
		// Push scope includes pull
		registryToken := generateRegistryToken(t, "build-123", []string{"builds/build-123"}, "push", time.Hour)
		basicAuth := base64.StdEncoding.EncodeToString([]byte(registryToken + ":"))

		req := httptest.NewRequest(http.MethodGet, "/v2/token?scope=repository:builds/build-123:pull", nil)
		req.Header.Set("Authorization", "Basic "+basicAuth)

		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)

		assert.Equal(t, http.StatusOK, rr.Code)
	})
}

func TestTokenHandler_NoAuth(t *testing.T) {
	handler := NewTokenHandler(testJWTSecret)

	t.Run("no auth returns 401", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/v2/token?scope=repository:builds/build-123:push", nil)
		req.RemoteAddr = "10.102.0.5:12345"

		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)

		assert.Equal(t, http.StatusUnauthorized, rr.Code)
	})

	t.Run("no auth returns WWW-Authenticate header for Basic auth challenge", func(t *testing.T) {
		// This test verifies that anonymous token requests get a proper auth challenge.
		// BuildKit needs this header to know it should retry with credentials.
		req := httptest.NewRequest(http.MethodGet, "/v2/token?scope=repository:cache/org-123:pull", nil)
		req.RemoteAddr = "172.30.0.5:12345"

		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)

		assert.Equal(t, http.StatusUnauthorized, rr.Code)

		// Verify WWW-Authenticate header is present and correct
		wwwAuth := rr.Header().Get("WWW-Authenticate")
		assert.NotEmpty(t, wwwAuth, "WWW-Authenticate header should be present on 401")
		assert.Contains(t, wwwAuth, "Basic", "should challenge with Basic auth")
		assert.Contains(t, wwwAuth, `realm="hypeman"`, "should include realm")
	})
}

func TestTokenHandler_ExpiredToken(t *testing.T) {
	handler := NewTokenHandler(testJWTSecret)

	t.Run("expired token returns 401", func(t *testing.T) {
		expiredToken := generateRegistryToken(t, "build-123", []string{"builds/build-123"}, "push", -time.Hour)
		basicAuth := base64.StdEncoding.EncodeToString([]byte(expiredToken + ":"))

		req := httptest.NewRequest(http.MethodGet, "/v2/token?scope=repository:builds/build-123:push", nil)
		req.Header.Set("Authorization", "Basic "+basicAuth)

		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)

		assert.Equal(t, http.StatusUnauthorized, rr.Code)
	})
}

func TestParseScope(t *testing.T) {
	tests := []struct {
		scope           string
		expectedRepo    string
		expectedActions []string
	}{
		{
			scope:           "repository:builds/abc123:push",
			expectedRepo:    "builds/abc123",
			expectedActions: []string{"push"},
		},
		{
			scope:           "repository:builds/abc123:push,pull",
			expectedRepo:    "builds/abc123",
			expectedActions: []string{"push", "pull"},
		},
		{
			scope:           "repository:myimage:pull",
			expectedRepo:    "myimage",
			expectedActions: []string{"pull"},
		},
		{
			scope:           "invalid",
			expectedRepo:    "",
			expectedActions: nil,
		},
		{
			scope:           "",
			expectedRepo:    "",
			expectedActions: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.scope, func(t *testing.T) {
			repo, actions := parseScope(tt.scope)
			assert.Equal(t, tt.expectedRepo, repo)
			assert.Equal(t, tt.expectedActions, actions)
		})
	}
}
