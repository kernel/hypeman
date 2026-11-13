package middleware

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/kernel/hypeman/lib/jwtauth"
	"github.com/kernel/hypeman/lib/scopes"
	"github.com/kernel/hypeman/lib/tokenrevocation"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/metric/noop"
)

func newTestVerifier(t *testing.T, primary string, fallback ...string) *jwtauth.Verifier {
	t.Helper()
	verifier, err := jwtauth.NewVerifier(primary, fallback, noop.NewMeterProvider().Meter("test"))
	require.NoError(t, err)
	return verifier
}

func TestJWTKeyRotation(t *testing.T) {
	for _, key := range []string{"old", "new"} {
		t.Run(key, func(t *testing.T) {
			verifier := newTestVerifier(t, "new", "old")
			sign := func(claims jwt.MapClaims) string {
				t.Helper()
				token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte(key))
				require.NoError(t, err)
				return token
			}
			user := sign(jwt.MapClaims{
				"sub": "user", "exp": time.Now().Add(time.Hour).Unix(),
				"permissions": []string{"instance:read"},
			})
			builder := sign(jwt.MapClaims{
				"sub": "builder-test", "build_id": "test", "exp": time.Now().Add(time.Hour).Unix(),
				"repo_access": []RepoPermission{{Repo: "builds/test", Scope: "pull"}},
			})
			expired := sign(jwt.MapClaims{"sub": "user", "exp": time.Now().Add(-time.Hour).Unix()})
			revoked, err := tokenrevocation.New([]string{tokenrevocation.Hash(user), tokenrevocation.Hash(builder)})
			require.NoError(t, err)

			for _, tc := range []struct {
				name, token, path, method string
				revoked                   *tokenrevocation.List
				status                    int
			}{
				{"user API", user, "/instances", http.MethodGet, nil, http.StatusOK},
				{"user registry", user, "/v2/", http.MethodGet, nil, http.StatusOK},
				{"builder pull", builder, "/v2/builds/test/manifests/latest", http.MethodGet, nil, http.StatusOK},
				{"builder cannot push", builder, "/v2/builds/test/manifests/latest", http.MethodPut, nil, http.StatusUnauthorized},
				{"builder cannot access another repo", builder, "/v2/builds/other/manifests/latest", http.MethodPut, nil, http.StatusUnauthorized},
				{"mirror fallback", builder, "/v2/builds/other/manifests/latest", http.MethodGet, nil, http.StatusBadGateway},
				{"builder cannot access API", builder, "/instances", http.MethodGet, nil, http.StatusUnauthorized},
				{"revoked user", user, "/instances", http.MethodGet, revoked, http.StatusUnauthorized},
				{"revoked registry user", user, "/v2/", http.MethodGet, revoked, http.StatusUnauthorized},
				{"revoked builder", builder, "/v2/builds/test/manifests/latest", http.MethodGet, revoked, http.StatusUnauthorized},
				{"expired", expired, "/instances", http.MethodGet, nil, http.StatusUnauthorized},
			} {
				t.Run(tc.name, func(t *testing.T) {
					handler := JwtAuth(verifier, tc.revoked)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						if tc.token == user {
							assert.True(t, scopes.HasScope(r.Context(), scopes.InstanceRead))
							assert.False(t, scopes.HasScope(r.Context(), scopes.InstanceWrite))
						}
						w.WriteHeader(http.StatusOK)
					}))
					headers := []string{"Bearer " + tc.token}
					if isRegistryPath(tc.path) {
						headers = append(headers, "Basic "+base64.StdEncoding.EncodeToString([]byte(tc.token+":")),
							"Basic "+base64.StdEncoding.EncodeToString([]byte(":"+tc.token)))
					}
					for _, header := range headers {
						req := httptest.NewRequest(tc.method, tc.path, nil)
						req.Header.Set("Authorization", header)
						rr := httptest.NewRecorder()
						handler.ServeHTTP(rr, req)
						assert.Equal(t, tc.status, rr.Code)
					}
				})
			}

			auth := OapiAuthenticationFunc(verifier, nil)
			input := newBearerAuthInput(t, user)
			require.NoError(t, auth(context.Background(), input))
			assert.True(t, scopes.HasScope(input.RequestValidationInput.Request.Context(), scopes.InstanceRead))
			assert.False(t, scopes.HasScope(input.RequestValidationInput.Request.Context(), scopes.InstanceWrite))
			assert.Error(t, auth(context.Background(), newBearerAuthInput(t, builder)))
			assert.Error(t, auth(context.Background(), newBearerAuthInput(t, expired)))
			assert.Error(t, OapiAuthenticationFunc(verifier, revoked)(context.Background(), newBearerAuthInput(t, user)))
		})
	}
}
