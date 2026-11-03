package registry

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/kernel/hypeman/lib/jwtauth"
	"github.com/kernel/hypeman/lib/middleware"
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

func TestRegistryKeyRotation(t *testing.T) {
	for _, phase := range []struct {
		name     string
		primary  string
		fallback []string
	}{
		{"overlap", "old", []string{"new"}},
		{"switched", "new", []string{"old"}},
		{"retired", "new", nil},
	} {
		t.Run(phase.name, func(t *testing.T) {
			verifier := newTestVerifier(t, phase.primary, phase.fallback...)
			for _, key := range []string{"old", "new", "unknown"} {
				for _, tokenType := range []string{"user", "builder", "expired", "revoked"} {
					t.Run(key+"/"+tokenType, func(t *testing.T) {
						claims := jwt.MapClaims{"sub": "user", "exp": time.Now().Add(time.Hour).Unix()}
						if tokenType == "builder" {
							claims["sub"] = "builder-test"
							claims["build_id"] = "test"
							claims["repo_access"] = []middleware.RepoPermission{{Repo: "builds/test", Scope: "push"}}
						}
						if tokenType == "expired" {
							claims["exp"] = time.Now().Add(-time.Hour).Unix()
						}
						token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte(key))
						require.NoError(t, err)
						var revoked *tokenrevocation.List
						if tokenType == "revoked" {
							revoked, err = tokenrevocation.New([]string{tokenrevocation.Hash(token)})
							require.NoError(t, err)
						}
						accepted := key != "unknown" && (key != "old" || phase.name != "retired") && tokenType != "expired" && tokenType != "revoked"
						handler := NewTokenHandler(verifier, revoked)
						for _, auth := range []string{"bearer", "basic username", "basic password"} {
							req := httptest.NewRequest(http.MethodGet, "/v2/token?scope=repository:builds/test:push,pull", nil)
							switch auth {
							case "bearer":
								req.Header.Set("Authorization", "Bearer "+token)
							case "basic username":
								req.SetBasicAuth(token, "")
							case "basic password":
								req.SetBasicAuth("", token)
							}
							rr := httptest.NewRecorder()
							handler.ServeHTTP(rr, req)
							if !accepted {
								assert.Equal(t, http.StatusUnauthorized, rr.Code)
								continue
							}
							require.Equal(t, http.StatusOK, rr.Code)
							var response TokenResponse
							require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &response))
							assert.Equal(t, token, response.Token, "token endpoint must not re-sign credentials")

							pull := httptest.NewRequest(http.MethodGet, "/v2/builds/test/manifests/latest", nil)
							pull.Header.Set("Authorization", "Bearer "+response.Token)
							pullResult := httptest.NewRecorder()
							middleware.JwtAuth(verifier, revoked)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
								w.WriteHeader(http.StatusOK)
							})).ServeHTTP(pullResult, pull)
							assert.Equal(t, http.StatusOK, pullResult.Code)
						}
					})
				}
			}
		})
	}
}
