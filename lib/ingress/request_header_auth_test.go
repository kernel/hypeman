package ingress

import (
	"context"
	"encoding/json"
	"os"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	testAuthEnv    = "HYPEMAN_INGRESS_AUTH_TEST"
	testAuthHeader = "X-Ingress-Verification"
	testAuthValue  = "test-secret-value"
)

func TestGenerateConfigWithRequestHeaderAuth(t *testing.T) {
	t.Setenv(testAuthEnv, testAuthValue)
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

func TestRequestHeaderAuthEnvironmentFailuresDoNotExposeValues(t *testing.T) {
	generator, _, cleanup := setupTestGenerator(t)
	defer cleanup()
	ingresses := []Ingress{protectedTestIngress()}

	original, existed := os.LookupEnv(testAuthEnv)
	require.NoError(t, os.Unsetenv(testAuthEnv))
	t.Cleanup(func() {
		if existed {
			_ = os.Setenv(testAuthEnv, original)
		} else {
			_ = os.Unsetenv(testAuthEnv)
		}
	})
	_, err := generator.GenerateConfig(context.Background(), ingresses)
	require.Error(t, err)
	assert.Contains(t, err.Error(), testAuthEnv)

	for _, value := range []string{"", "secret\nDO_NOT_EXPOSE", "secret*wildcard"} {
		require.NoError(t, os.Setenv(testAuthEnv, value))
		_, err = generator.GenerateConfig(context.Background(), ingresses)
		require.Error(t, err)
		assert.Contains(t, err.Error(), testAuthEnv)
		if value != "" {
			assert.NotContains(t, err.Error(), value)
		}
		assert.NotContains(t, err.Error(), "DO_NOT_EXPOSE")
	}
}

func TestCreateIngressRejectsMissingRequestHeaderAuthEnvironment(t *testing.T) {
	manager, _, _, cleanup := setupTestManager(t)
	defer cleanup()
	original, existed := os.LookupEnv(testAuthEnv)
	require.NoError(t, os.Unsetenv(testAuthEnv))
	t.Cleanup(func() {
		if existed {
			_ = os.Setenv(testAuthEnv, original)
		}
	})
	ingress := protectedTestIngress()
	ingress.Rules[0].Target.Instance = "my-api"
	_, err := manager.Create(context.Background(), CreateIngressRequest{Name: "protected", Rules: ingress.Rules})
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrInvalidRequest)
	assert.Contains(t, err.Error(), testAuthEnv)
}

func TestRequestHeaderAuthReferenceValidation(t *testing.T) {
	const secret = "DO_NOT_EXPOSE"
	t.Setenv(testAuthEnv, secret)
	reserved := []string{
		"Host", "Authorization", "Cookie", "Proxy-Authorization", "Content-Length",
		"Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Connection", "TE",
		"Trailer", "Transfer-Encoding", "Upgrade", "Sec-WebSocket-Key",
	}
	for _, header := range reserved {
		auth := &RequestHeaderAuth{Header: header, SecretEnv: testAuthEnv}
		err := auth.Validate()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "reserved")
		assert.NotContains(t, err.Error(), secret)
	}

	for _, auth := range []*RequestHeaderAuth{
		{Header: "Bad Header", SecretEnv: testAuthEnv},
		{Header: testAuthHeader, SecretEnv: "OTHER_SECRET"},
		{Header: testAuthHeader, SecretEnv: "HYPEMAN_INGRESS_AUTH_lowercase"},
		{Header: testAuthHeader, SecretEnv: "HYPEMAN_INGRESS_AUTH_"},
	} {
		err := auth.Validate()
		require.Error(t, err)
		assert.NotContains(t, err.Error(), secret)
	}
}

func TestRequestHeaderAuthPersistenceAndBackwardCompatibility(t *testing.T) {
	generator, p, cleanup := setupTestGenerator(t)
	defer cleanup()
	t.Setenv(testAuthEnv, testAuthValue)

	stored := &storedIngress{
		ID: "protected", Name: "protected", CreatedAt: "2025-01-15T10:00:00Z",
		Rules: protectedTestIngress().Rules,
	}
	require.NoError(t, saveIngress(p, stored))
	metadata, err := os.ReadFile(p.IngressMetadata(stored.ID))
	require.NoError(t, err)
	assert.Contains(t, string(metadata), testAuthHeader)
	assert.Contains(t, string(metadata), testAuthEnv)
	assert.NotContains(t, string(metadata), testAuthValue)
	loaded, err := loadIngress(p, stored.ID)
	require.NoError(t, err)
	require.NotNil(t, loaded.Rules[0].RequestHeaderAuth)
	assert.Equal(t, testAuthEnv, loaded.Rules[0].RequestHeaderAuth.SecretEnv)

	legacy := `{"id":"legacy","name":"legacy","rules":[{"match":{"hostname":"legacy.example.com"},"target":{"instance":"legacy","port":8080}}],"created_at":"2025-01-15T10:00:00Z"}`
	require.NoError(t, os.WriteFile(p.IngressMetadata("legacy"), []byte(legacy), 0644))
	loaded, err = loadIngress(p, "legacy")
	require.NoError(t, err)
	assert.Nil(t, loaded.Rules[0].RequestHeaderAuth)

	require.NoError(t, generator.WriteConfig(context.Background(), []Ingress{protectedTestIngress()}))
	info, err := os.Stat(p.CaddyConfig())
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0600), info.Mode().Perm())
}

func protectedTestIngress() Ingress {
	return Ingress{
		ID: "protected", Name: "protected",
		Rules: []IngressRule{{
			Match:             IngressMatch{Hostname: "service.example.com", Port: 443},
			Target:            IngressTarget{Instance: "service", Port: 8080},
			TLS:               true,
			RedirectHTTP:      true,
			RequestHeaderAuth: &RequestHeaderAuth{Header: testAuthHeader, SecretEnv: testAuthEnv},
		}},
	}
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
