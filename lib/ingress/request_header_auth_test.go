package ingress

import (
	"context"
	"encoding/json"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	testAuthHeader = "X-Ingress-Verification"
	testAuthValue  = "0123456789abcdef0123456789abcdef"
)

func TestGenerateConfigWithRequestHeaderAuth(t *testing.T) {
	generator, _, cleanup := setupTestGenerator(t)
	defer cleanup()

	data, err := generator.GenerateConfig(context.Background(), []Ingress{protectedTestIngress()})
	require.NoError(t, err)
	routes := configRoutes(t, data, 443)
	require.Len(t, routes, 3)

	authorized := routes[0].(map[string]interface{})
	matcher := authorized["match"].([]interface{})[0].(map[string]interface{})
	assert.Equal(t, []interface{}{"service.example.com"}, matcher["host"])
	headerMatcher := matcher["header"].(map[string]interface{})
	assert.Equal(t, []interface{}{testAuthValue}, headerMatcher[testAuthHeader])

	handlers := authorized["handle"].([]interface{})
	require.Len(t, handlers, 2)
	deleteHandler := handlers[0].(map[string]interface{})
	assert.Equal(t, "headers", deleteHandler["handler"])
	requestHeaders := deleteHandler["request"].(map[string]interface{})
	assert.Equal(t, []interface{}{testAuthHeader}, requestHeaders["delete"])
	proxyHandler := handlers[1].(map[string]interface{})
	assert.Equal(t, "reverse_proxy", proxyHandler["handler"])
	assert.Contains(t, proxyHandler, "dynamic_upstreams")
	assert.NotContains(t, proxyHandler, "request_buffers")
	assert.NotContains(t, proxyHandler, "response_buffers")

	denial := routes[1].(map[string]interface{})
	denialMatcher := denial["match"].([]interface{})[0].(map[string]interface{})
	assert.Equal(t, []interface{}{"service.example.com"}, denialMatcher["host"])
	assert.NotContains(t, denialMatcher, "header")
	denialHandler := denial["handle"].([]interface{})[0].(map[string]interface{})
	assert.EqualValues(t, 403, denialHandler["status_code"])
	assert.Equal(t, "static_response", denialHandler["handler"])
	assert.NotContains(t, denialHandler, "dynamic_upstreams")

	redirects := configRoutes(t, data, 80)
	redirect := redirects[0].(map[string]interface{})
	redirectMatcher := redirect["match"].([]interface{})[0].(map[string]interface{})
	assert.NotContains(t, redirectMatcher, "header")
	redirectHandler := redirect["handle"].([]interface{})[0].(map[string]interface{})
	assert.EqualValues(t, 301, redirectHandler["status_code"])
}

func TestGenerateConfigWithoutRequestHeaderAuthKeepsRouteShape(t *testing.T) {
	generator, _, cleanup := setupTestGenerator(t)
	defer cleanup()
	ingress := protectedTestIngress()
	ingress.Rules[0].RequestHeaderAuth = nil

	data, err := generator.GenerateConfig(context.Background(), []Ingress{ingress})
	require.NoError(t, err)
	routes := configRoutes(t, data, 443)
	require.Len(t, routes, 2)
	route := routes[0].(map[string]interface{})
	matcher := route["match"].([]interface{})[0].(map[string]interface{})
	assert.Equal(t, map[string]interface{}{"host": []interface{}{"service.example.com"}}, matcher)
	handlers := route["handle"].([]interface{})
	require.Len(t, handlers, 1)
	assert.Equal(t, "reverse_proxy", handlers[0].(map[string]interface{})["handler"])
}

func TestRequestHeaderAuthValidationDoesNotExposeValues(t *testing.T) {
	for _, value := range []string{"", strings.Repeat("a", 32) + "\nDO_NOT_EXPOSE", strings.Repeat("a", 32) + "*"} {
		auth := &RequestHeaderAuth{Header: testAuthHeader, Value: value}
		err := auth.Validate()
		require.Error(t, err)
		if value != "" {
			assert.NotContains(t, err.Error(), value)
		}
		assert.NotContains(t, err.Error(), "DO_NOT_EXPOSE")
	}
}

func TestRequestHeaderAuthValueLengthBoundaries(t *testing.T) {
	tests := []struct {
		length int
		valid  bool
	}{
		{length: 31, valid: false},
		{length: 32, valid: true},
		{length: 256, valid: true},
		{length: 257, valid: false},
	}

	for _, tt := range tests {
		t.Run(strconv.Itoa(tt.length), func(t *testing.T) {
			auth := &RequestHeaderAuth{Header: testAuthHeader, Value: strings.Repeat("a", tt.length)}
			err := auth.Validate()
			if tt.valid {
				require.NoError(t, err)
				return
			}
			require.EqualError(t, err, "request_header_auth.value must be 32-256 bytes of visible ASCII without Caddy matcher metacharacters")
		})
	}
}

func TestCreateIngressRejectsInvalidRequestHeaderAuthValue(t *testing.T) {
	manager, _, _, cleanup := setupTestManager(t)
	defer cleanup()
	ingress := protectedTestIngress()
	ingress.Rules[0].Target.Instance = "my-api"
	ingress.Rules[0].RequestHeaderAuth.Value = "too-short"
	_, err := manager.Create(context.Background(), CreateIngressRequest{Name: "protected", Rules: ingress.Rules})
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrInvalidRequest)
	assert.NotContains(t, err.Error(), "too-short")
}

func TestRequestHeaderAuthFieldValidation(t *testing.T) {
	reserved := []string{
		"Host", "Authorization", "Cookie", "Proxy-Authorization", "Content-Length",
		"Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Connection", "TE",
		"Trailer", "Transfer-Encoding", "Upgrade", "Sec-WebSocket-Key",
	}
	for _, header := range reserved {
		auth := &RequestHeaderAuth{Header: header, Value: testAuthValue}
		err := auth.Validate()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "reserved")
		assert.NotContains(t, err.Error(), testAuthValue)
	}

	for _, auth := range []*RequestHeaderAuth{
		{Header: "Bad Header", Value: testAuthValue},
		{Header: testAuthHeader, Value: strings.Repeat("a", 31)},
		{Header: testAuthHeader, Value: strings.Repeat("a", 257)},
	} {
		err := auth.Validate()
		require.Error(t, err)
		assert.NotContains(t, err.Error(), auth.Value)
	}
}

func TestRequestHeaderAuthPersistenceAndBackwardCompatibility(t *testing.T) {
	generator, p, cleanup := setupTestGenerator(t)
	defer cleanup()

	stored := &storedIngress{
		ID: "protected", Name: "protected", CreatedAt: "2025-01-15T10:00:00Z",
		Rules: protectedTestIngress().Rules,
	}
	require.NoError(t, saveIngress(p, stored))
	metadata, err := os.ReadFile(p.IngressMetadata(stored.ID))
	require.NoError(t, err)
	assert.Contains(t, string(metadata), testAuthHeader)
	assert.Contains(t, string(metadata), testAuthValue)
	info, err := os.Stat(p.IngressMetadata(stored.ID))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0600), info.Mode().Perm())

	loaded, err := loadIngress(p, stored.ID)
	require.NoError(t, err)
	require.NotNil(t, loaded.Rules[0].RequestHeaderAuth)
	assert.Equal(t, testAuthValue, loaded.Rules[0].RequestHeaderAuth.Value)

	legacy := `{"id":"legacy","name":"legacy","rules":[{"match":{"hostname":"legacy.example.com"},"target":{"instance":"legacy","port":8080}}],"created_at":"2025-01-15T10:00:00Z"}`
	require.NoError(t, os.WriteFile(p.IngressMetadata("legacy"), []byte(legacy), 0644))
	loaded, err = loadIngress(p, "legacy")
	require.NoError(t, err)
	assert.Nil(t, loaded.Rules[0].RequestHeaderAuth)

	require.NoError(t, generator.WriteConfig(context.Background(), []Ingress{protectedTestIngress()}))
	info, err = os.Stat(p.CaddyConfig())
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0600), info.Mode().Perm())
}

func TestRequestHeaderAuthExactRoutePrecedesPatternForBothCreationOrders(t *testing.T) {
	orders := []struct {
		name  string
		first CreateIngressRequest
		last  CreateIngressRequest
	}{
		{name: "pattern then exact", first: unprotectedPatternRequest(), last: protectedExactRequest()},
		{name: "exact then pattern", first: protectedExactRequest(), last: unprotectedPatternRequest()},
	}

	for _, tt := range orders {
		t.Run(tt.name, func(t *testing.T) {
			manager, _, p, cleanup := setupTestManager(t)
			defer cleanup()
			ctx := context.Background()

			_, err := manager.Create(ctx, tt.first)
			require.NoError(t, err)
			_, err = manager.Create(ctx, tt.last)
			require.NoError(t, err)

			data, err := os.ReadFile(p.CaddyConfig())
			require.NoError(t, err)
			assertProtectedExactBeforePattern(t, configRoutes(t, data, 80))
		})
	}
}

func TestRequestHeaderAuthExactRoutePrecedesPatternAfterPersistedReload(t *testing.T) {
	generator, p, cleanup := setupTestGenerator(t)
	defer cleanup()

	pattern := unprotectedPatternRequest()
	exact := protectedExactRequest()
	require.NoError(t, saveIngress(p, &storedIngress{ID: "a-pattern", Name: pattern.Name, Rules: pattern.Rules, CreatedAt: "2025-01-15T10:00:00Z"}))
	require.NoError(t, saveIngress(p, &storedIngress{ID: "z-exact", Name: exact.Name, Rules: exact.Rules, CreatedAt: "2025-01-15T10:00:00Z"}))

	stored, err := loadAllIngresses(p)
	require.NoError(t, err)
	require.Len(t, stored, 2)
	require.True(t, stored[0].Rules[0].Match.IsPattern())

	ingresses := make([]Ingress, 0, len(stored))
	for i := range stored {
		ingresses = append(ingresses, *storedToIngress(&stored[i]))
	}
	data, err := generator.GenerateConfig(context.Background(), ingresses)
	require.NoError(t, err)
	assertProtectedExactBeforePattern(t, configRoutes(t, data, 80))
}

func protectedTestIngress() Ingress {
	return Ingress{
		ID: "protected", Name: "protected",
		Rules: []IngressRule{{
			Match:             IngressMatch{Hostname: "service.example.com", Port: 443},
			Target:            IngressTarget{Instance: "service", Port: 8080},
			TLS:               true,
			RedirectHTTP:      true,
			RequestHeaderAuth: &RequestHeaderAuth{Header: testAuthHeader, Value: testAuthValue},
		}},
	}
}

func protectedExactRequest() CreateIngressRequest {
	return CreateIngressRequest{
		Name: "protected-exact",
		Rules: []IngressRule{{
			Match:             IngressMatch{Hostname: "admin.example.com"},
			Target:            IngressTarget{Instance: "my-api", Port: 8080},
			RequestHeaderAuth: &RequestHeaderAuth{Header: testAuthHeader, Value: testAuthValue},
		}},
	}
}

func unprotectedPatternRequest() CreateIngressRequest {
	return CreateIngressRequest{
		Name: "unprotected-pattern",
		Rules: []IngressRule{{
			Match:  IngressMatch{Hostname: "{instance}.example.com"},
			Target: IngressTarget{Instance: "{instance}", Port: 8080},
		}},
	}
}

func assertProtectedExactBeforePattern(t *testing.T, routes []interface{}) {
	t.Helper()
	require.Len(t, routes, 4)
	assert.Equal(t, "admin.example.com", routeHostname(t, routes[0]))
	assert.Equal(t, "admin.example.com", routeHostname(t, routes[1]))
	assert.Equal(t, "*.example.com", routeHostname(t, routes[2]))

	authorizedMatcher := routes[0].(map[string]interface{})["match"].([]interface{})[0].(map[string]interface{})
	assert.Contains(t, authorizedMatcher, "header")
	denialHandler := routes[1].(map[string]interface{})["handle"].([]interface{})[0].(map[string]interface{})
	assert.EqualValues(t, 403, denialHandler["status_code"])
}

func routeHostname(t *testing.T, route interface{}) string {
	t.Helper()
	matcher := route.(map[string]interface{})["match"].([]interface{})[0].(map[string]interface{})
	hosts := matcher["host"].([]interface{})
	require.Len(t, hosts, 1)
	return hosts[0].(string)
}

func configRoutes(t *testing.T, data []byte, port int) []interface{} {
	t.Helper()
	var config map[string]interface{}
	require.NoError(t, json.Unmarshal(data, &config))
	apps := config["apps"].(map[string]interface{})
	httpApp := apps["http"].(map[string]interface{})
	servers := httpApp["servers"].(map[string]interface{})
	server := servers["ingress-"+strconv.Itoa(port)].(map[string]interface{})
	return server["routes"].([]interface{})
}
