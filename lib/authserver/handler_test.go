package authserver

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func postToken(server *Server, form url.Values, alter func(*http.Request)) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodPost, tokenPath, strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if alter != nil {
		alter(request)
	}
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)
	return recorder
}

func TestTokenEndpoint(t *testing.T) {
	f := newFixture(t, nil)
	form := func(identity string) url.Values {
		return url.Values{"grant_type": {GrantTypeJWTBearer}, "assertion": {identity}, "federation_rule_id": {"ci"}}
	}
	response := postToken(f.server, form(f.identity(t, nil)), nil)
	require.Equal(t, http.StatusOK, response.Code)
	require.Equal(t, "no-store", response.Header().Get("Cache-Control"))
	var body map[string]any
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &body))
	require.Equal(t, "Bearer", body["token_type"])
	require.EqualValues(t, 300, body["expires_in"])
	require.Equal(t, "instance:read instance:write", body["scope"])
	_, err := f.resource(t, f.server, testAudience).Verify(t.Context(), body["access_token"].(string))
	require.NoError(t, err)

	withResource := form(f.identity(t, nil))
	withResource.Set("resource", testAudience)
	require.Equal(t, http.StatusOK, postToken(f.server, withResource, nil).Code)

	for name, tc := range map[string]struct {
		form   func() url.Values
		alter  func(*http.Request)
		status int
		code   string
	}{
		"not form-encoded":         {func() url.Values { return form("x") }, func(r *http.Request) { r.Header.Set("Content-Type", "application/json") }, 400, "invalid_request"},
		"no grant type":            {func() url.Values { v := form("x"); v.Del("grant_type"); return v }, nil, 400, "invalid_request"},
		"another grant type":       {func() url.Values { v := form("x"); v.Set("grant_type", "client_credentials"); return v }, nil, 400, "unsupported_grant_type"},
		"another grant type alone": {func() url.Values { return url.Values{"grant_type": {"client_credentials"}} }, nil, 400, "unsupported_grant_type"},
		"no identity token":        {func() url.Values { v := form("x"); v.Del("assertion"); return v }, nil, 400, "invalid_request"},
		"no rule":                  {func() url.Values { v := form("x"); v.Del("federation_rule_id"); return v }, nil, 400, "invalid_request"},
		"unknown parameter":        {func() url.Values { v := form("x"); v.Set("scope", "instance:read"); return v }, nil, 400, "invalid_request"},
		"repeated parameter":       {func() url.Values { v := form("x"); v.Add("federation_rule_id", "other"); return v }, nil, 400, "invalid_request"},
		"client authentication":    {func() url.Values { return form("x") }, func(r *http.Request) { r.Header.Set("Authorization", "Bearer x") }, 400, "invalid_request"},
		// The request as a whole is judged before its grant type is.
		"another grant type with an unknown parameter": {func() url.Values {
			v := form("x")
			v.Set("grant_type", "client_credentials")
			v.Set("client_secret", "x")
			return v
		}, nil, 400, "invalid_request"},
		// A parameter in the URL would be ignored, so it is refused: here the
		// caller asks for an audience the rule does not have.
		"parameter in the URL":  {func() url.Values { return form(f.identity(t, nil)) }, func(r *http.Request) { r.URL.RawQuery = "resource=prod" }, 400, "invalid_request"},
		"too large":             {func() url.Values { return form(strings.Repeat("a", maxTokenRequestBytes)) }, nil, 400, "invalid_request"},
		"identity not accepted": {func() url.Values { return form("x") }, nil, 400, "invalid_grant"},
		"unknown rule":          {func() url.Values { v := form(f.identity(t, nil)); v.Set("federation_rule_id", "other"); return v }, nil, 400, "invalid_grant"},
		"audience not allowed":  {func() url.Values { v := form(f.identity(t, nil)); v.Set("resource", "prod"); return v }, nil, 400, "invalid_target"},
	} {
		t.Run(name, func(t *testing.T) {
			response := postToken(f.server, tc.form(), tc.alter)
			require.Equal(t, tc.status, response.Code)
			var body map[string]string
			require.NoError(t, json.Unmarshal(response.Body.Bytes(), &body))
			require.Equal(t, tc.code, body["error"])
			require.NotEmpty(t, body["error_description"])
			require.Equal(t, "no-store", response.Header().Get("Cache-Control"))
		})
	}

	f.spent.err = http.ErrHandlerTimeout
	response = postToken(f.server, form(f.identity(t, nil)), nil)
	require.Equal(t, http.StatusServiceUnavailable, response.Code)
	require.Contains(t, response.Body.String(), "temporarily_unavailable")
	require.NotContains(t, response.Body.String(), http.ErrHandlerTimeout.Error(), "the cause stays in the log")

	request := httptest.NewRequest(http.MethodGet, tokenPath, nil)
	recorder := httptest.NewRecorder()
	f.server.Handler().ServeHTTP(recorder, request)
	require.Equal(t, http.StatusMethodNotAllowed, recorder.Code)
}

// A Hypeman server finds the key set through the metadata, and the key set
// lets it verify what the token endpoint issues.
func TestMetadataAndKeySet(t *testing.T) {
	f := newFixture(t, nil)
	get := func(path string) (*httptest.ResponseRecorder, map[string]any) {
		recorder := httptest.NewRecorder()
		f.server.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
		var body map[string]any
		if recorder.Code == http.StatusOK {
			require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &body))
		}
		return recorder, body
	}
	response, metadata := get(metadataPath)
	require.Equal(t, http.StatusOK, response.Code)
	require.Equal(t, "application/json", response.Header().Get("Content-Type"))
	require.Equal(t, testIssuer, metadata["issuer"])
	require.Equal(t, testIssuer+"/oauth/token", metadata["token_endpoint"])
	require.Equal(t, testIssuer+"/.well-known/jwks.json", metadata["jwks_uri"])
	require.Equal(t, []any{GrantTypeJWTBearer}, metadata["grant_types_supported"])
	require.Equal(t, "no-cache", response.Header().Get("Cache-Control"))

	response, set := get(keySetPath)
	require.Equal(t, http.StatusOK, response.Code)
	require.Len(t, set["keys"], 1)
	require.Equal(t, f.server.key.ID, set["keys"].([]any)[0].(map[string]any)["kid"])

	response, _ = get("/healthz")
	require.Equal(t, http.StatusNoContent, response.Code)
	response, _ = get("/metrics")
	require.Equal(t, http.StatusNotFound, response.Code, "metrics are not served to callers")
}
