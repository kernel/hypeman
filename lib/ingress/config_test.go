package ingress

import (
	"context"
	"encoding/json"
	"os"
	"testing"

	"github.com/onkernel/hypeman/lib/paths"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func setupTestGenerator(t *testing.T) (*CaddyConfigGenerator, *paths.Paths, func()) {
	t.Helper()

	// Create temp dir
	tmpDir, err := os.MkdirTemp("", "ingress-config-test-*")
	require.NoError(t, err)

	p := paths.New(tmpDir)

	// Create required directories
	require.NoError(t, os.MkdirAll(p.CaddyDir(), 0755))
	require.NoError(t, os.MkdirAll(p.CaddyDataDir(), 0755))

	// Empty ACMEConfig means TLS is not configured
	// Use DNS resolver port for dynamic upstreams
	dnsResolverPort := 5353
	generator := NewCaddyConfigGenerator(p, "0.0.0.0", "127.0.0.1", 2019, ACMEConfig{}, dnsResolverPort)

	cleanup := func() {
		os.RemoveAll(tmpDir)
	}

	return generator, p, cleanup
}

func TestGenerateConfig_EmptyIngresses(t *testing.T) {
	generator, _, cleanup := setupTestGenerator(t)
	defer cleanup()

	ctx := context.Background()
	ingresses := []Ingress{}

	data, err := generator.GenerateConfig(ctx, ingresses)
	require.NoError(t, err)

	// Parse JSON to verify structure
	var config map[string]interface{}
	err = json.Unmarshal(data, &config)
	require.NoError(t, err)

	// Should have admin section
	admin, ok := config["admin"].(map[string]interface{})
	require.True(t, ok, "config should have admin section")
	assert.Equal(t, "127.0.0.1:2019", admin["listen"])

	// Should NOT have apps section when no ingresses exist
	// (no HTTP server started until ingresses are created)
	_, hasApps := config["apps"]
	assert.False(t, hasApps, "config should not have apps section with no ingresses")

	// Should have storage section pointing to data directory
	storage, ok := config["storage"].(map[string]interface{})
	require.True(t, ok, "config should have storage section")
	assert.Equal(t, "file_system", storage["module"])
	// Verify storage root is set (path will vary based on temp dir)
	root, ok := storage["root"].(string)
	require.True(t, ok, "storage should have root path")
	assert.Contains(t, root, "caddy/data", "storage root should be caddy data directory")
}

func TestGenerateConfig_StoragePath(t *testing.T) {
	// Test that the storage path is correctly configured based on the paths
	tmpDir, err := os.MkdirTemp("", "ingress-storage-test-*")
	require.NoError(t, err)
	defer os.RemoveAll(tmpDir)

	p := paths.New(tmpDir)
	require.NoError(t, os.MkdirAll(p.CaddyDir(), 0755))
	require.NoError(t, os.MkdirAll(p.CaddyDataDir(), 0755))

	generator := NewCaddyConfigGenerator(p, "0.0.0.0", "127.0.0.1", 2019, ACMEConfig{}, 5353)

	ctx := context.Background()
	data, err := generator.GenerateConfig(ctx, []Ingress{})
	require.NoError(t, err)

	var config map[string]interface{}
	err = json.Unmarshal(data, &config)
	require.NoError(t, err)

	// Verify storage configuration
	storage := config["storage"].(map[string]interface{})
	assert.Equal(t, "file_system", storage["module"])
	assert.Equal(t, p.CaddyDataDir(), storage["root"], "storage root should match CaddyDataDir")

	// Verify the path structure is correct
	// CaddyDataDir should be under CaddyDir
	expectedDataDir := tmpDir + "/caddy/data"
	assert.Equal(t, expectedDataDir, p.CaddyDataDir(), "CaddyDataDir should be under data directory")
}

func TestGenerateConfig_SingleIngress(t *testing.T) {
	generator, _, cleanup := setupTestGenerator(t)
	defer cleanup()

	ingresses := []Ingress{
		{
			ID:   "ing-123",
			Name: "my-ingress",
			Rules: []IngressRule{
				{
					Match: IngressMatch{
						Hostname: "api.example.com",
					},
					Target: IngressTarget{
						Instance: "my-api",
						Port:     8080,
					},
				},
			},
		},
	}

	ctx := context.Background()

	data, err := generator.GenerateConfig(ctx, ingresses)
	require.NoError(t, err)

	configStr := string(data)

	// Verify key elements are present
	assert.Contains(t, configStr, "api.example.com", "config should contain hostname")
	assert.Contains(t, configStr, "dynamic_upstreams", "config should use dynamic upstreams")
	assert.Contains(t, configStr, "reverse_proxy", "config should contain reverse_proxy handler")
	assert.Contains(t, configStr, "my-api", "config should contain instance name in upstream URL")
	assert.Contains(t, configStr, "8080", "config should contain target port")

	// Verify catch-all 404 route is present
	assert.Contains(t, configStr, "static_response", "config should contain static_response handler for 404")
	assert.Contains(t, configStr, "no ingress configured for hostname", "config should contain 404 message")
}

func TestGenerateConfig_MultipleRules(t *testing.T) {
	generator, _, cleanup := setupTestGenerator(t)
	defer cleanup()

	ctx := context.Background()
	ingresses := []Ingress{
		{
			ID:   "ing-123",
			Name: "multi-rule-ingress",
			Rules: []IngressRule{
				{
					Match:  IngressMatch{Hostname: "api.example.com"},
					Target: IngressTarget{Instance: "api-service", Port: 8080},
				},
				{
					Match:  IngressMatch{Hostname: "web.example.com"},
					Target: IngressTarget{Instance: "web-service", Port: 3000},
				},
			},
		},
	}

	data, err := generator.GenerateConfig(ctx, ingresses)
	require.NoError(t, err)

	configStr := string(data)

	// Verify both hosts are present
	assert.Contains(t, configStr, "api.example.com")
	assert.Contains(t, configStr, "web.example.com")
	assert.Contains(t, configStr, "api-service")
	assert.Contains(t, configStr, "web-service")
}

func TestGenerateConfig_MultipleIngresses(t *testing.T) {
	generator, _, cleanup := setupTestGenerator(t)
	defer cleanup()

	ctx := context.Background()
	ingresses := []Ingress{
		{
			ID:    "ing-1",
			Name:  "ingress-1",
			Rules: []IngressRule{{Match: IngressMatch{Hostname: "app1.example.com"}, Target: IngressTarget{Instance: "app1", Port: 8080}}},
		},
		{
			ID:    "ing-2",
			Name:  "ingress-2",
			Rules: []IngressRule{{Match: IngressMatch{Hostname: "app2.example.com"}, Target: IngressTarget{Instance: "app2", Port: 9000}}},
		},
	}

	data, err := generator.GenerateConfig(ctx, ingresses)
	require.NoError(t, err)

	configStr := string(data)

	// Verify all hosts and instances are present
	assert.Contains(t, configStr, "app1.example.com")
	assert.Contains(t, configStr, "app2.example.com")
	assert.Contains(t, configStr, "app1")
	assert.Contains(t, configStr, "app2")
}

func TestGenerateConfig_MultiplePorts(t *testing.T) {
	generator, _, cleanup := setupTestGenerator(t)
	defer cleanup()

	ctx := context.Background()
	ingresses := []Ingress{
		{
			ID:   "ing-1",
			Name: "port-80-ingress",
			Rules: []IngressRule{
				{Match: IngressMatch{Hostname: "api.example.com", Port: 80}, Target: IngressTarget{Instance: "api", Port: 8080}},
			},
		},
		{
			ID:   "ing-2",
			Name: "port-8080-ingress",
			Rules: []IngressRule{
				{Match: IngressMatch{Hostname: "internal.example.com", Port: 8080}, Target: IngressTarget{Instance: "internal", Port: 3000}},
			},
		},
		{
			ID:   "ing-3",
			Name: "port-9000-ingress",
			Rules: []IngressRule{
				{Match: IngressMatch{Hostname: "metrics.example.com", Port: 9000}, Target: IngressTarget{Instance: "metrics", Port: 9090}},
			},
		},
	}

	data, err := generator.GenerateConfig(ctx, ingresses)
	require.NoError(t, err)

	configStr := string(data)

	// Verify listen addresses include all ports
	assert.Contains(t, configStr, ":80")
	assert.Contains(t, configStr, ":8080")
	assert.Contains(t, configStr, ":9000")

	// Verify all hostnames are present
	assert.Contains(t, configStr, "api.example.com")
	assert.Contains(t, configStr, "internal.example.com")
	assert.Contains(t, configStr, "metrics.example.com")
}

func TestGenerateConfig_DeterministicOrder(t *testing.T) {
	generator, _, cleanup := setupTestGenerator(t)
	defer cleanup()

	ctx := context.Background()
	// Create ingresses with ports in non-sorted order to verify output is deterministic
	ingresses := []Ingress{
		{
			ID:   "ing-1",
			Name: "high-port-ingress",
			Rules: []IngressRule{
				{Match: IngressMatch{Hostname: "metrics.example.com", Port: 9000}, Target: IngressTarget{Instance: "metrics", Port: 9090}},
			},
		},
		{
			ID:   "ing-2",
			Name: "low-port-ingress",
			Rules: []IngressRule{
				{Match: IngressMatch{Hostname: "api.example.com", Port: 80}, Target: IngressTarget{Instance: "api", Port: 8080}},
			},
		},
		{
			ID:   "ing-3",
			Name: "mid-port-ingress",
			Rules: []IngressRule{
				{Match: IngressMatch{Hostname: "internal.example.com", Port: 443}, Target: IngressTarget{Instance: "internal", Port: 3000}},
			},
		},
	}

	// Generate config multiple times and verify output is identical
	var firstOutput []byte
	for i := 0; i < 5; i++ {
		data, err := generator.GenerateConfig(ctx, ingresses)
		require.NoError(t, err)

		if firstOutput == nil {
			firstOutput = data
		} else {
			assert.Equal(t, string(firstOutput), string(data), "config output should be deterministic on iteration %d", i)
		}
	}

	// Also verify the listen addresses are in sorted order (80, 443, 9000)
	var config map[string]interface{}
	err := json.Unmarshal(firstOutput, &config)
	require.NoError(t, err)

	apps := config["apps"].(map[string]interface{})
	httpApp := apps["http"].(map[string]interface{})
	servers := httpApp["servers"].(map[string]interface{})
	ingressServer := servers["ingress"].(map[string]interface{})
	listenAddrs := ingressServer["listen"].([]interface{})

	require.Len(t, listenAddrs, 3)
	assert.Equal(t, "0.0.0.0:80", listenAddrs[0].(string))
	assert.Equal(t, "0.0.0.0:443", listenAddrs[1].(string))
	assert.Equal(t, "0.0.0.0:9000", listenAddrs[2].(string))
}

func TestGenerateConfig_DefaultPort(t *testing.T) {
	generator, _, cleanup := setupTestGenerator(t)
	defer cleanup()

	ctx := context.Background()
	// Test that Port=0 defaults to 80
	ingresses := []Ingress{
		{
			ID:   "ing-1",
			Name: "default-port-ingress",
			Rules: []IngressRule{
				{Match: IngressMatch{Hostname: "api.example.com", Port: 0}, Target: IngressTarget{Instance: "api", Port: 8080}},
			},
		},
	}

	data, err := generator.GenerateConfig(ctx, ingresses)
	require.NoError(t, err)

	configStr := string(data)

	// Should create listener on port 80 (default)
	assert.Contains(t, configStr, "0.0.0.0:80")
}

func TestWriteConfig(t *testing.T) {
	generator, p, cleanup := setupTestGenerator(t)
	defer cleanup()

	ctx := context.Background()
	ingresses := []Ingress{
		{
			ID:    "ing-123",
			Name:  "test-ingress",
			Rules: []IngressRule{{Match: IngressMatch{Hostname: "test.example.com"}, Target: IngressTarget{Instance: "test-svc", Port: 8080}}},
		},
	}

	err := generator.WriteConfig(ctx, ingresses)
	require.NoError(t, err)

	// Verify config file was written
	configPath := p.CaddyConfig()
	data, err := os.ReadFile(configPath)
	require.NoError(t, err)
	assert.True(t, len(data) > 0, "config file should not be empty")
	assert.Contains(t, string(data), "test.example.com")
	assert.Contains(t, string(data), "test-svc")
}

func TestConfigIsValidJSON(t *testing.T) {
	generator, _, cleanup := setupTestGenerator(t)
	defer cleanup()

	ctx := context.Background()
	ingresses := []Ingress{
		{
			ID:   "ing-123",
			Name: "test-ingress",
			Rules: []IngressRule{
				{
					Match:  IngressMatch{Hostname: "api.example.com"},
					Target: IngressTarget{Instance: "my-api", Port: 8080},
				},
			},
		},
	}

	data, err := generator.GenerateConfig(ctx, ingresses)
	require.NoError(t, err)

	// Verify it's valid JSON by parsing it
	var config interface{}
	err = json.Unmarshal(data, &config)
	require.NoError(t, err, "generated config should be valid JSON")
}

func TestGenerateConfig_WithTLS(t *testing.T) {
	// Create temp dir
	tmpDir, err := os.MkdirTemp("", "ingress-config-tls-test-*")
	require.NoError(t, err)
	defer os.RemoveAll(tmpDir)

	p := paths.New(tmpDir)
	require.NoError(t, os.MkdirAll(p.CaddyDir(), 0755))
	require.NoError(t, os.MkdirAll(p.CaddyDataDir(), 0755))

	// Create generator with ACME configured
	acmeConfig := ACMEConfig{
		Email:              "admin@example.com",
		DNSProvider:        DNSProviderCloudflare,
		CloudflareAPIToken: "test-token",
	}
	generator := NewCaddyConfigGenerator(p, "0.0.0.0", "127.0.0.1", 2019, acmeConfig, 5353)

	ctx := context.Background()
	ingresses := []Ingress{
		{
			ID:   "ing-123",
			Name: "tls-ingress",
			Rules: []IngressRule{
				{
					Match:        IngressMatch{Hostname: "secure.example.com", Port: 443},
					Target:       IngressTarget{Instance: "my-api", Port: 8080},
					TLS:          true,
					RedirectHTTP: true,
				},
			},
		},
	}

	data, err := generator.GenerateConfig(ctx, ingresses)
	require.NoError(t, err)

	configStr := string(data)

	// Verify TLS automation is configured
	assert.Contains(t, configStr, "tls", "config should contain tls section")
	assert.Contains(t, configStr, "automation", "config should contain automation")
	assert.Contains(t, configStr, "secure.example.com", "config should contain hostname")
	assert.Contains(t, configStr, "acme", "config should contain acme issuer")
	assert.Contains(t, configStr, "cloudflare", "config should contain cloudflare provider")
	assert.Contains(t, configStr, "admin@example.com", "config should contain email")

	// Verify HTTP redirect route is created
	assert.Contains(t, configStr, "301", "config should contain redirect status")
	assert.Contains(t, configStr, "Location", "config should contain Location header")
}

func TestGenerateConfig_WithTLSDisabled(t *testing.T) {
	generator, _, cleanup := setupTestGenerator(t)
	defer cleanup()

	ctx := context.Background()
	ingresses := []Ingress{
		{
			ID:   "ing-123",
			Name: "no-tls-ingress",
			Rules: []IngressRule{
				{
					Match:  IngressMatch{Hostname: "api.example.com"},
					Target: IngressTarget{Instance: "my-api", Port: 8080},
					TLS:    false,
				},
			},
		},
	}

	data, err := generator.GenerateConfig(ctx, ingresses)
	require.NoError(t, err)

	configStr := string(data)

	// Verify TLS automation is NOT present when disabled
	assert.NotContains(t, configStr, `"automation"`, "config should not contain tls automation when disabled")
}

func TestACMEConfig_IsTLSConfigured(t *testing.T) {
	tests := []struct {
		name     string
		config   ACMEConfig
		expected bool
	}{
		{
			name:     "empty config",
			config:   ACMEConfig{},
			expected: false,
		},
		{
			name: "cloudflare configured",
			config: ACMEConfig{
				Email:              "admin@example.com",
				DNSProvider:        DNSProviderCloudflare,
				CloudflareAPIToken: "token",
			},
			expected: true,
		},
		{
			name: "cloudflare missing token",
			config: ACMEConfig{
				Email:       "admin@example.com",
				DNSProvider: DNSProviderCloudflare,
			},
			expected: false,
		},
		{
			name: "no provider set",
			config: ACMEConfig{
				Email:       "admin@example.com",
				DNSProvider: DNSProviderNone,
			},
			expected: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result := tc.config.IsTLSConfigured()
			assert.Equal(t, tc.expected, result)
		})
	}
}

func TestACMEConfig_IsDomainAllowed(t *testing.T) {
	tests := []struct {
		name           string
		allowedDomains string
		hostname       string
		expected       bool
	}{
		{
			name:           "empty config - no domains allowed",
			allowedDomains: "",
			hostname:       "example.com",
			expected:       false,
		},
		{
			name:           "exact match",
			allowedDomains: "api.example.com",
			hostname:       "api.example.com",
			expected:       true,
		},
		{
			name:           "exact match with multiple patterns",
			allowedDomains: "api.example.com, www.example.com, admin.example.com",
			hostname:       "www.example.com",
			expected:       true,
		},
		{
			name:           "wildcard match",
			allowedDomains: "*.example.com",
			hostname:       "api.example.com",
			expected:       true,
		},
		{
			name:           "wildcard match - different subdomain",
			allowedDomains: "*.example.com",
			hostname:       "www.example.com",
			expected:       true,
		},
		{
			name:           "wildcard does not match nested subdomains",
			allowedDomains: "*.example.com",
			hostname:       "api.v2.example.com",
			expected:       false,
		},
		{
			name:           "wildcard does not match apex domain",
			allowedDomains: "*.example.com",
			hostname:       "example.com",
			expected:       false,
		},
		{
			name:           "no match - wrong domain",
			allowedDomains: "*.example.com",
			hostname:       "api.other.com",
			expected:       false,
		},
		{
			name:           "no match - similar but different domain",
			allowedDomains: "*.hypeman-development.com",
			hostname:       "test.hypeman-developments.com",
			expected:       false,
		},
		{
			name:           "multiple patterns with wildcard",
			allowedDomains: "*.example.com, api.other.com",
			hostname:       "api.other.com",
			expected:       true,
		},
		{
			name:           "whitespace handling",
			allowedDomains: "  *.example.com  ,  api.other.com  ",
			hostname:       "api.other.com",
			expected:       true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			config := ACMEConfig{AllowedDomains: tc.allowedDomains}
			result := config.IsDomainAllowed(tc.hostname)
			assert.Equal(t, tc.expected, result, "hostname=%q, allowed=%q", tc.hostname, tc.allowedDomains)
		})
	}
}

func TestGenerateConfig_MixedTLSAndNonTLS(t *testing.T) {
	// Create temp dir
	tmpDir, err := os.MkdirTemp("", "ingress-config-mixed-tls-test-*")
	require.NoError(t, err)
	defer os.RemoveAll(tmpDir)

	p := paths.New(tmpDir)
	require.NoError(t, os.MkdirAll(p.CaddyDir(), 0755))
	require.NoError(t, os.MkdirAll(p.CaddyDataDir(), 0755))

	// Create generator with ACME configured
	acmeConfig := ACMEConfig{
		Email:              "admin@example.com",
		DNSProvider:        DNSProviderCloudflare,
		CloudflareAPIToken: "test-token",
	}
	generator := NewCaddyConfigGenerator(p, "0.0.0.0", "127.0.0.1", 2019, acmeConfig, 5353)

	ctx := context.Background()
	ingresses := []Ingress{
		{
			ID:   "mixed-ingress",
			Name: "mixed-ingress",
			Rules: []IngressRule{
				{
					// Non-TLS rule on port 80
					Match:  IngressMatch{Hostname: "api.example.com", Port: 80},
					Target: IngressTarget{Instance: "api", Port: 8080},
					TLS:    false,
				},
				{
					// TLS rule on port 443
					Match:        IngressMatch{Hostname: "secure.example.com", Port: 443},
					Target:       IngressTarget{Instance: "secure", Port: 8080},
					TLS:          true,
					RedirectHTTP: true,
				},
			},
		},
	}

	data, err := generator.GenerateConfig(ctx, ingresses)
	require.NoError(t, err)

	configStr := string(data)

	// Verify both hostnames are present
	assert.Contains(t, configStr, "api.example.com")
	assert.Contains(t, configStr, "secure.example.com")

	// Verify TLS automation is configured for secure hostname
	assert.Contains(t, configStr, "automation")
	assert.Contains(t, configStr, "acme")

	// Verify HTTP redirect is present (for TLS rule with redirect_http)
	assert.Contains(t, configStr, "301")

	// Verify automatic_https has disable_redirects (not fully disabled)
	// because we have TLS hostnames
	assert.Contains(t, configStr, `"disable_redirects"`)
	assert.NotContains(t, configStr, `"disable": true`)
}

func TestHasTLSRules(t *testing.T) {
	tests := []struct {
		name      string
		ingresses []Ingress
		expected  bool
	}{
		{
			name:      "empty",
			ingresses: []Ingress{},
			expected:  false,
		},
		{
			name: "no TLS",
			ingresses: []Ingress{
				{Rules: []IngressRule{{TLS: false}}},
			},
			expected: false,
		},
		{
			name: "with TLS",
			ingresses: []Ingress{
				{Rules: []IngressRule{{TLS: true}}},
			},
			expected: true,
		},
		{
			name: "mixed",
			ingresses: []Ingress{
				{Rules: []IngressRule{{TLS: false}}},
				{Rules: []IngressRule{{TLS: true}}},
			},
			expected: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result := HasTLSRules(tc.ingresses)
			assert.Equal(t, tc.expected, result)
		})
	}
}

func TestGenerateConfig_PatternHostname(t *testing.T) {
	generator, _, cleanup := setupTestGenerator(t)
	defer cleanup()

	ctx := context.Background()
	ingresses := []Ingress{
		{
			ID:   "pattern-ingress",
			Name: "pattern-ingress",
			Rules: []IngressRule{
				{
					Match:  IngressMatch{Hostname: "{instance}.example.com"},
					Target: IngressTarget{Instance: "{instance}", Port: 8080},
				},
			},
		},
	}

	data, err := generator.GenerateConfig(ctx, ingresses)
	require.NoError(t, err)

	configStr := string(data)

	// Verify wildcard is used for hostname matching
	assert.Contains(t, configStr, "*.example.com")

	// Verify dynamic upstream uses Caddy placeholder for instance resolution
	assert.Contains(t, configStr, "http.request.host.labels")
}

func TestGenerateConfig_DynamicUpstreams(t *testing.T) {
	// Create temp dir
	tmpDir, err := os.MkdirTemp("", "ingress-config-dynamic-test-*")
	require.NoError(t, err)
	defer os.RemoveAll(tmpDir)

	p := paths.New(tmpDir)
	require.NoError(t, os.MkdirAll(p.CaddyDir(), 0755))
	require.NoError(t, os.MkdirAll(p.CaddyDataDir(), 0755))

	dnsPort := 5353
	generator := NewCaddyConfigGenerator(p, "0.0.0.0", "127.0.0.1", 2019, ACMEConfig{}, dnsPort)

	ctx := context.Background()
	ingresses := []Ingress{
		{
			ID:   "ing-123",
			Name: "test-ingress",
			Rules: []IngressRule{
				{
					Match:  IngressMatch{Hostname: "api.example.com"},
					Target: IngressTarget{Instance: "my-api", Port: 8080},
				},
			},
		},
	}

	data, err := generator.GenerateConfig(ctx, ingresses)
	require.NoError(t, err)

	configStr := string(data)

	// Verify DNS-based dynamic upstreams structure is present
	assert.Contains(t, configStr, "dynamic_upstreams")
	assert.Contains(t, configStr, `"source"`)
	assert.Contains(t, configStr, `"a"`)

	// Verify DNS hostname and resolver are configured
	assert.Contains(t, configStr, "my-api.hypeman.internal")
	assert.Contains(t, configStr, "resolver")
	assert.Contains(t, configStr, "127.0.0.1:5353")
}
