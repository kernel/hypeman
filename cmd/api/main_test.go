package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/go-chi/chi/v5"
	"github.com/golang-jwt/jwt/v5"
	nethttpmiddleware "github.com/oapi-codegen/nethttp-middleware"
	mw "github.com/kernel/hypeman/lib/middleware"
	"github.com/kernel/hypeman/lib/oapi"
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
