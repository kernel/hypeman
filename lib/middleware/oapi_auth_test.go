package middleware

import (
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testJWTSecret = "test-secret-key-for-testing"

// generateUserToken creates a valid user JWT token
func generateUserToken(t *testing.T, userID string) string {
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"sub": userID,
		"iat": time.Now().Unix(),
		"exp": time.Now().Add(time.Hour).Unix(),
	})
	tokenString, err := token.SignedString([]byte(testJWTSecret))
	require.NoError(t, err)
	return tokenString
}

// generateRegistryToken creates a registry token using the legacy format
func generateRegistryToken(t *testing.T, buildID string) string {
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"sub":      "builder-" + buildID,
		"iat":      time.Now().Unix(),
		"exp":      time.Now().Add(time.Hour).Unix(),
		"iss":      "hypeman",
		"build_id": buildID,
		"repos":    []string{"builds/" + buildID},
		"scope":    "push",
	})
	tokenString, err := token.SignedString([]byte(testJWTSecret))
	require.NoError(t, err)
	return tokenString
}

// generateRepoAccessToken creates a registry token using the new RepoAccess format
func generateRepoAccessToken(t *testing.T, buildID string, repoAccess []map[string]string) string {
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"sub":         "builder-" + buildID,
		"iat":         time.Now().Unix(),
		"exp":         time.Now().Add(time.Hour).Unix(),
		"iss":         "hypeman",
		"build_id":    buildID,
		"repo_access": repoAccess,
	})
	tokenString, err := token.SignedString([]byte(testJWTSecret))
	require.NoError(t, err)
	return tokenString
}

func TestJwtAuth_RejectsRegistryTokens(t *testing.T) {
	// Create a simple handler that returns 200 if auth passes
	nextHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	// Wrap with JwtAuth middleware
	handler := JwtAuth(testJWTSecret)(nextHandler)

	t.Run("valid user token is accepted", func(t *testing.T) {
		userToken := generateUserToken(t, "user-123")

		req := httptest.NewRequest(http.MethodGet, "/instances", nil)
		req.Header.Set("Authorization", "Bearer "+userToken)

		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)

		assert.Equal(t, http.StatusOK, rr.Code, "user token should be accepted")
	})

	t.Run("registry token with repos claim is rejected", func(t *testing.T) {
		registryToken := generateRegistryToken(t, "build-abc123")

		req := httptest.NewRequest(http.MethodGet, "/instances", nil)
		req.Header.Set("Authorization", "Bearer "+registryToken)

		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)

		assert.Equal(t, http.StatusUnauthorized, rr.Code, "registry token should be rejected")
		assert.Contains(t, rr.Body.String(), "invalid token type")
	})

	t.Run("token with only builder- prefix is rejected", func(t *testing.T) {
		// A token that has builder- prefix but no other registry claims
		// This could be crafted by an attacker who knows the pattern
		token := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
			"sub": "builder-malicious-build",
			"iat": time.Now().Unix(),
			"exp": time.Now().Add(time.Hour).Unix(),
		})
		tokenString, err := token.SignedString([]byte(testJWTSecret))
		require.NoError(t, err)

		req := httptest.NewRequest(http.MethodGet, "/instances", nil)
		req.Header.Set("Authorization", "Bearer "+tokenString)

		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)

		assert.Equal(t, http.StatusUnauthorized, rr.Code, "builder- prefix token should be rejected")
		assert.Contains(t, rr.Body.String(), "invalid token type")
	})

	t.Run("token with scope claim is rejected", func(t *testing.T) {
		token := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
			"sub":   "some-user",
			"iat":   time.Now().Unix(),
			"exp":   time.Now().Add(time.Hour).Unix(),
			"scope": "push",
		})
		tokenString, err := token.SignedString([]byte(testJWTSecret))
		require.NoError(t, err)

		req := httptest.NewRequest(http.MethodGet, "/instances", nil)
		req.Header.Set("Authorization", "Bearer "+tokenString)

		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)

		assert.Equal(t, http.StatusUnauthorized, rr.Code, "token with scope claim should be rejected")
	})

	t.Run("token with build_id claim is rejected", func(t *testing.T) {
		token := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
			"sub":      "some-user",
			"iat":      time.Now().Unix(),
			"exp":      time.Now().Add(time.Hour).Unix(),
			"build_id": "some-build",
		})
		tokenString, err := token.SignedString([]byte(testJWTSecret))
		require.NoError(t, err)

		req := httptest.NewRequest(http.MethodGet, "/instances", nil)
		req.Header.Set("Authorization", "Bearer "+tokenString)

		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)

		assert.Equal(t, http.StatusUnauthorized, rr.Code, "token with build_id claim should be rejected")
	})
}

func TestExtractRepoFromPath(t *testing.T) {
	tests := []struct {
		name     string
		path     string
		expected string
	}{
		// Simple repository names
		{
			name:     "simple repo with manifests",
			path:     "/v2/test-alpine/manifests/latest",
			expected: "test-alpine",
		},
		{
			name:     "simple repo with blobs",
			path:     "/v2/test-alpine/blobs/sha256:abc123",
			expected: "test-alpine",
		},
		{
			name:     "simple repo with uploads",
			path:     "/v2/test-alpine/blobs/uploads/uuid-here",
			expected: "test-alpine",
		},

		// Nested repository names (like builds/abc123)
		{
			name:     "nested repo with manifests",
			path:     "/v2/builds/abc123/manifests/latest",
			expected: "builds/abc123",
		},
		{
			name:     "nested repo with blobs",
			path:     "/v2/builds/abc123/blobs/sha256:def456",
			expected: "builds/abc123",
		},
		{
			name:     "nested repo with uploads",
			path:     "/v2/builds/abc123/blobs/uploads/uuid-here",
			expected: "builds/abc123",
		},

		// Base path (no repo)
		{
			name:     "base path",
			path:     "/v2/",
			expected: "",
		},

		// Edge cases
		{
			name:     "repo named manifests-test",
			path:     "/v2/manifests-test/manifests/latest",
			expected: "manifests-test",
		},
		{
			name:     "repo named blobs-data",
			path:     "/v2/blobs-data/blobs/sha256:abc",
			expected: "blobs-data",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := extractRepoFromPath(tt.path)
			assert.Equal(t, tt.expected, result, "extractRepoFromPath(%q)", tt.path)
		})
	}
}

func TestValidateRegistryToken(t *testing.T) {
	t.Run("legacy format token allows push to allowed repo", func(t *testing.T) {
		token := generateRegistryToken(t, "build-123")
		claims, err := validateRegistryToken(token, testJWTSecret, "/v2/builds/build-123/manifests/latest", http.MethodPut)
		require.NoError(t, err)
		assert.Equal(t, "build-123", claims.BuildID)
	})

	t.Run("legacy format token rejects unauthorized repo", func(t *testing.T) {
		token := generateRegistryToken(t, "build-123")
		_, err := validateRegistryToken(token, testJWTSecret, "/v2/builds/other-build/manifests/latest", http.MethodPut)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "not allowed by token")
	})

	t.Run("RepoAccess format token allows push to push-scoped repo", func(t *testing.T) {
		repoAccess := []map[string]string{
			{"repo": "builds/build-456", "scope": "push"},
			{"repo": "cache/tenant-x", "scope": "push"},
			{"repo": "cache/global/node", "scope": "pull"},
		}
		token := generateRepoAccessToken(t, "build-456", repoAccess)

		claims, err := validateRegistryToken(token, testJWTSecret, "/v2/builds/build-456/manifests/latest", http.MethodPut)
		require.NoError(t, err)
		assert.Equal(t, "build-456", claims.BuildID)
	})

	t.Run("RepoAccess format token allows pull from pull-scoped repo", func(t *testing.T) {
		repoAccess := []map[string]string{
			{"repo": "builds/build-456", "scope": "push"},
			{"repo": "cache/global/node", "scope": "pull"},
		}
		token := generateRepoAccessToken(t, "build-456", repoAccess)

		// GET (pull) from pull-scoped repo should work
		_, err := validateRegistryToken(token, testJWTSecret, "/v2/cache/global/node/manifests/latest", http.MethodGet)
		require.NoError(t, err)
	})

	t.Run("RepoAccess format token rejects push to pull-only repo", func(t *testing.T) {
		repoAccess := []map[string]string{
			{"repo": "builds/build-456", "scope": "push"},
			{"repo": "cache/global/node", "scope": "pull"},
		}
		token := generateRepoAccessToken(t, "build-456", repoAccess)

		// PUT (push) to pull-only repo should fail
		_, err := validateRegistryToken(token, testJWTSecret, "/v2/cache/global/node/manifests/latest", http.MethodPut)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "does not allow write operations")
	})

	t.Run("RepoAccess format token rejects unauthorized repo", func(t *testing.T) {
		repoAccess := []map[string]string{
			{"repo": "builds/build-456", "scope": "push"},
		}
		token := generateRepoAccessToken(t, "build-456", repoAccess)

		_, err := validateRegistryToken(token, testJWTSecret, "/v2/builds/other-build/manifests/latest", http.MethodPut)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "not allowed by token")
	})

	t.Run("allows base path check without repo validation", func(t *testing.T) {
		token := generateRegistryToken(t, "build-123")
		_, err := validateRegistryToken(token, testJWTSecret, "/v2/", http.MethodGet)
		require.NoError(t, err)
	})
}

func TestJwtAuth_RegistryPaths(t *testing.T) {
	// Create a simple handler that returns 200 if auth passes
	nextHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	handler := JwtAuth(testJWTSecret)(nextHandler)

	t.Run("valid registry token allows access to authorized repo", func(t *testing.T) {
		token := generateRegistryToken(t, "build-123")

		req := httptest.NewRequest(http.MethodHead, "/v2/builds/build-123/manifests/latest", nil)
		req.Header.Set("Authorization", "Basic "+basicAuth(token))
		req.RemoteAddr = "8.8.8.8:12345" // External IP - should still work with valid token

		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)

		assert.Equal(t, http.StatusOK, rr.Code, "valid registry token should allow access")
	})

	t.Run("valid RepoAccess token allows access to authorized repo", func(t *testing.T) {
		repoAccess := []map[string]string{
			{"repo": "builds/build-456", "scope": "push"},
			{"repo": "cache/tenant-x", "scope": "push"},
		}
		token := generateRepoAccessToken(t, "build-456", repoAccess)

		req := httptest.NewRequest(http.MethodPut, "/v2/builds/build-456/manifests/latest", nil)
		req.Header.Set("Authorization", "Basic "+basicAuth(token))
		req.RemoteAddr = "8.8.8.8:12345" // External IP

		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)

		assert.Equal(t, http.StatusOK, rr.Code, "valid RepoAccess token should allow access")
	})

	t.Run("no token but internal staging IP allows access via fallback", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodHead, "/v2/builds/any-build/manifests/latest", nil)
		// No Authorization header
		req.RemoteAddr = "10.100.5.50:12345" // Staging subnet

		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)

		assert.Equal(t, http.StatusOK, rr.Code, "internal staging IP should allow access via fallback")
	})

	t.Run("no token but internal production IP allows access via fallback", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodHead, "/v2/builds/any-build/manifests/latest", nil)
		// No Authorization header
		req.RemoteAddr = "172.30.16.101:42700" // Production subnet

		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)

		// BuildKit with insecure registries doesn't do WWW-Authenticate challenge-response,
		// so we need IP fallback for production
		assert.Equal(t, http.StatusOK, rr.Code, "internal production IP should allow access via fallback")
	})

	t.Run("no token and external IP returns 401 with WWW-Authenticate header", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodHead, "/v2/builds/any-build/manifests/latest", nil)
		// No Authorization header
		req.RemoteAddr = "8.8.8.8:12345" // External IP

		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)

		assert.Equal(t, http.StatusUnauthorized, rr.Code, "external IP without token should be rejected")
		assert.Contains(t, rr.Body.String(), "registry authentication required")
		// WWW-Authenticate header is required for Docker/BuildKit to send credentials
		// We use Bearer auth with a token endpoint for standard Docker registry auth flow
		wwwAuth := rr.Header().Get("WWW-Authenticate")
		assert.Contains(t, wwwAuth, `Bearer realm="`,
			"401 response must include WWW-Authenticate Bearer header for Docker auth")
	})

	t.Run("invalid token but internal IP allows access via fallback", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodHead, "/v2/builds/any-build/manifests/latest", nil)
		req.Header.Set("Authorization", "Basic "+basicAuth("invalid-not-a-jwt-token"))
		req.RemoteAddr = "10.102.1.50:12345" // Internal IP

		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)

		assert.Equal(t, http.StatusOK, rr.Code, "internal IP should allow access via fallback even with invalid token")
	})

	t.Run("invalid token and external IP returns 401", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodHead, "/v2/builds/any-build/manifests/latest", nil)
		req.Header.Set("Authorization", "Basic "+basicAuth("invalid-not-a-jwt-token"))
		req.RemoteAddr = "8.8.8.8:12345" // External IP

		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)

		assert.Equal(t, http.StatusUnauthorized, rr.Code, "external IP with invalid token should be rejected")
	})

	t.Run("valid token for wrong repo returns 401 even with internal IP", func(t *testing.T) {
		token := generateRegistryToken(t, "build-123")

		req := httptest.NewRequest(http.MethodPut, "/v2/builds/different-build/manifests/latest", nil)
		req.Header.Set("Authorization", "Basic "+basicAuth(token))
		req.RemoteAddr = "10.100.5.50:12345" // Internal IP - but token validation fails first

		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)

		// Token is valid JWT but wrong repo - should fall through to IP fallback
		assert.Equal(t, http.StatusOK, rr.Code, "should fall back to IP check when token doesn't match repo")
	})

	t.Run("registry base path /v2/ allows access with valid token", func(t *testing.T) {
		token := generateRegistryToken(t, "build-123")

		req := httptest.NewRequest(http.MethodGet, "/v2/", nil)
		req.Header.Set("Authorization", "Basic "+basicAuth(token))
		req.RemoteAddr = "8.8.8.8:12345"

		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)

		assert.Equal(t, http.StatusOK, rr.Code, "/v2/ base path should be allowed with valid token")
	})

	t.Run("Bearer auth also works for registry paths", func(t *testing.T) {
		token := generateRegistryToken(t, "build-789")

		req := httptest.NewRequest(http.MethodHead, "/v2/builds/build-789/manifests/latest", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		req.RemoteAddr = "8.8.8.8:12345"

		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)

		assert.Equal(t, http.StatusOK, rr.Code, "Bearer auth should also work for registry paths")
	})

	t.Run("simulates BuildKit auth flow: 401 then retry with credentials", func(t *testing.T) {
		// This simulates what BuildKit should do:
		// 1. First request without auth -> 401 with WWW-Authenticate
		// 2. Second request with auth -> 200

		token := generateRegistryToken(t, "build-flow-test")

		// Step 1: Request without auth (external IP)
		req1 := httptest.NewRequest(http.MethodHead, "/v2/builds/build-flow-test/manifests/latest", nil)
		req1.RemoteAddr = "8.8.8.8:12345" // External IP, no fallback

		rr1 := httptest.NewRecorder()
		handler.ServeHTTP(rr1, req1)

		assert.Equal(t, http.StatusUnauthorized, rr1.Code, "first request without auth should get 401")
		wwwAuth := rr1.Header().Get("WWW-Authenticate")
		assert.Contains(t, wwwAuth, `Bearer realm="`,
			"401 must include WWW-Authenticate Bearer header to trigger client auth")

		// Step 2: Retry with Basic auth (what Docker/BuildKit does after seeing WWW-Authenticate)
		req2 := httptest.NewRequest(http.MethodHead, "/v2/builds/build-flow-test/manifests/latest", nil)
		req2.Header.Set("Authorization", "Basic "+basicAuth(token))
		req2.RemoteAddr = "8.8.8.8:12345"

		rr2 := httptest.NewRecorder()
		handler.ServeHTTP(rr2, req2)

		assert.Equal(t, http.StatusOK, rr2.Code, "retry with auth should succeed")
	})
}

// basicAuth creates a Basic auth value (base64 of "token:")
func basicAuth(token string) string {
	return base64.StdEncoding.EncodeToString([]byte(token + ":"))
}

func TestIsInternalVMRequest(t *testing.T) {
	tests := []struct {
		name       string
		remoteAddr string
		expected   bool
	}{
		// Staging/dev subnets
		{"staging 10.100.x.x", "10.100.1.50:12345", true},
		{"staging 10.102.x.x", "10.102.5.100:54321", true},

		// Production subnet (fallback needed because BuildKit doesn't do WWW-Authenticate)
		{"production 172.30.x.x", "172.30.16.101:42700", true},
		{"production 172.30.0.x", "172.30.0.50:8080", true},

		// External IPs (should be rejected)
		{"external 192.168.x.x", "192.168.1.100:8080", false},
		{"external public IP", "34.21.1.136:8080", false},
		{"external 10.0.x.x (different subnet)", "10.0.1.50:8080", false},
		{"external 172.16.x.x (different subnet)", "172.16.1.50:8080", false},

		// Edge cases
		{"localhost", "127.0.0.1:8080", false},
		{"IPv6 localhost", "[::1]:8080", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/v2/test/manifests/latest", nil)
			req.RemoteAddr = tt.remoteAddr

			result := isInternalVMRequest(req)
			assert.Equal(t, tt.expected, result, "isInternalVMRequest with RemoteAddr=%q", tt.remoteAddr)
		})
	}
}

func TestJwtAuth_RequiresAuthorization(t *testing.T) {
	nextHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	handler := JwtAuth(testJWTSecret)(nextHandler)

	t.Run("missing authorization header is rejected", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/instances", nil)

		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)

		assert.Equal(t, http.StatusUnauthorized, rr.Code)
		assert.Contains(t, rr.Body.String(), "authorization header required")
	})

	t.Run("invalid token format is rejected", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/instances", nil)
		req.Header.Set("Authorization", "Basic abc123")

		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)

		assert.Equal(t, http.StatusUnauthorized, rr.Code)
		assert.Contains(t, rr.Body.String(), "invalid authorization header format")
	})

	t.Run("expired token is rejected", func(t *testing.T) {
		token := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
			"sub": "user-123",
			"iat": time.Now().Add(-2 * time.Hour).Unix(),
			"exp": time.Now().Add(-1 * time.Hour).Unix(), // Expired
		})
		tokenString, err := token.SignedString([]byte(testJWTSecret))
		require.NoError(t, err)

		req := httptest.NewRequest(http.MethodGet, "/instances", nil)
		req.Header.Set("Authorization", "Bearer "+tokenString)

		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)

		assert.Equal(t, http.StatusUnauthorized, rr.Code)
		assert.Contains(t, rr.Body.String(), "invalid token")
	})

	t.Run("wrong secret is rejected", func(t *testing.T) {
		token := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
			"sub": "user-123",
			"iat": time.Now().Unix(),
			"exp": time.Now().Add(time.Hour).Unix(),
		})
		tokenString, err := token.SignedString([]byte("wrong-secret"))
		require.NoError(t, err)

		req := httptest.NewRequest(http.MethodGet, "/instances", nil)
		req.Header.Set("Authorization", "Bearer "+tokenString)

		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)

		assert.Equal(t, http.StatusUnauthorized, rr.Code)
		assert.Contains(t, rr.Body.String(), "invalid token")
	})
}
