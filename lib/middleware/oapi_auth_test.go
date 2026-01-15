package middleware

import (
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

// generateRegistryToken creates a registry token (like those given to builder VMs)
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

