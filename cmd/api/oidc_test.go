package main

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/go-chi/chi/v5"
	"github.com/golang-jwt/jwt/v5"
	"github.com/kernel/hypeman/lib/jwtauth"
	mw "github.com/kernel/hypeman/lib/middleware"
	"github.com/kernel/hypeman/lib/oapi"
	"github.com/kernel/hypeman/lib/oidcfederation"
	"github.com/kernel/hypeman/lib/scopes"
	nethttpmiddleware "github.com/oapi-codegen/nethttp-middleware"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/metric/noop"
)

// oidcTestServer wires the token endpoint and an authenticated API route the
// way run() does, for an issuer whose public key is configured inline.
func oidcTestServer(t *testing.T, key *rsa.PublicKey, permissions []string) http.Handler {
	t.Helper()
	registry, err := oidcfederation.Compile(oidcfederation.Config{
		Issuers: []oidcfederation.IssuerSpec{{
			Name:      "idp",
			IssuerURL: "https://idp.example.test",
			// A server refuses single-use assertions while it has just started.
			TokenValidation: oidcfederation.TokenValidation{ReplayProtection: oidcfederation.ReplayAllowReuse},
			JWKS: oidcfederation.KeySource{Type: "inline", Keys: []map[string]any{{
				"kty": "RSA", "kid": "key-a", "alg": "RS256", "use": "sig",
				"n": base64.RawURLEncoding.EncodeToString(key.N.Bytes()),
				"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes()),
			}}},
		}},
		FederationRules: []oidcfederation.RuleSpec{{
			Name: "deploy", Issuer: "idp", ExpectedAudience: "https://hypeman.example.test",
			Match:         oidcfederation.Match{Type: "custom_oidc", Subject: "deployer"},
			Authorization: oidcfederation.RuleAuthorization{Permissions: permissions},
		}},
	})
	require.NoError(t, err)
	meter := noop.NewMeterProvider().Meter("test")
	exchanger, err := oidcfederation.NewExchanger(registry, testJWTSecret, meter)
	require.NoError(t, err)
	verifier, err := jwtauth.NewVerifier(testJWTSecret, nil, meter)
	require.NoError(t, err)
	verifier = verifier.WithFederation(exchanger)
	spec, err := oapi.GetSwagger()
	require.NoError(t, err)
	spec.Servers = nil

	r := chi.NewRouter()
	r.Post("/oauth/token", exchanger.ServeHTTP)
	r.Group(func(r chi.Router) {
		r.Use(nethttpmiddleware.OapiRequestValidatorWithOptions(spec, &nethttpmiddleware.Options{
			Options:      openapi3filter.Options{AuthenticationFunc: mw.OapiAuthenticationFunc(verifier, nil)},
			ErrorHandler: mw.OapiErrorHandler,
		}))
		r.Use(scopes.Middleware())
		r.Get("/images", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`[]`)) })
		r.Post("/images", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusCreated) })
	})
	return r
}

func TestOIDCExchangedTokenAuthenticatesWithItsRulesScopes(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	server := oidcTestServer(t, &key.PublicKey, []string{"image:read"})

	assertion := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.MapClaims{
		"iss": "https://idp.example.test", "sub": "deployer", "aud": "https://hypeman.example.test",
		"iat": time.Now().Unix(), "exp": time.Now().Add(5 * time.Minute).Unix(),
	})
	assertion.Header["kid"] = "key-a"
	signed, err := assertion.SignedString(key)
	require.NoError(t, err)

	exchange := func(server http.Handler) *httptest.ResponseRecorder {
		form := url.Values{"grant_type": {oidcfederation.GrantTypeJWTBearer}, "assertion": {signed}, "federation_rule_id": {"deploy"}}
		request := httptest.NewRequest(http.MethodPost, "/oauth/token", strings.NewReader(form.Encode()))
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		recorder := httptest.NewRecorder()
		server.ServeHTTP(recorder, request)
		return recorder
	}
	call := func(server http.Handler, method, token string) int {
		body := ""
		if method == http.MethodPost {
			body = `{"name":"docker.io/library/alpine:latest"}`
		}
		request := httptest.NewRequest(method, "/images", strings.NewReader(body))
		request.Header.Set("Authorization", "Bearer "+token)
		request.Header.Set("Content-Type", "application/json")
		recorder := httptest.NewRecorder()
		server.ServeHTTP(recorder, request)
		return recorder.Code
	}

	issued := exchange(server)
	require.Equal(t, http.StatusOK, issued.Code, issued.Body.String())
	var response struct {
		AccessToken string `json:"access_token"`
		Scope       string `json:"scope"`
	}
	require.NoError(t, json.Unmarshal(issued.Body.Bytes(), &response))
	assert.Equal(t, "image:read", response.Scope)

	assert.Equal(t, http.StatusOK, call(server, http.MethodGet, response.AccessToken))
	assert.Equal(t, http.StatusForbidden, call(server, http.MethodPost, response.AccessToken), "the token holds only its rule's permissions")

	// The same rule name with different permissions is a different revision:
	// a server restarted with it rejects the token issued before the change.
	changed := oidcTestServer(t, &key.PublicKey, []string{"image:read", "image:write"})
	assert.Equal(t, http.StatusUnauthorized, call(changed, http.MethodGet, response.AccessToken))
}
