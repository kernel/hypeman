package ingress

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"testing"

	"github.com/kernel/hypeman/lib/paths"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestPatternParsing tests the hostname pattern parsing functionality.
func TestPatternParsing(t *testing.T) {
	t.Run("IsPattern", func(t *testing.T) {
		tests := []struct {
			hostname string
			expected bool
		}{
			{"api.example.com", false},
			{"{instance}.example.com", true},
			{"{app}-{env}.example.com", true},
			{"*.example.com", false}, // Raw wildcards are not patterns
			{"foo.bar.example.com", false},
		}

		for _, tc := range tests {
			match := IngressMatch{Hostname: tc.hostname}
			assert.Equal(t, tc.expected, match.IsPattern(), "IsPattern for %q", tc.hostname)
		}
	})

	t.Run("ParsePattern_Simple", func(t *testing.T) {
		match := IngressMatch{Hostname: "{instance}.example.com"}
		pattern, err := match.ParsePattern()
		require.NoError(t, err)

		assert.Equal(t, "{instance}.example.com", pattern.Original)
		assert.Equal(t, "*.example.com", pattern.Wildcard)
		assert.Equal(t, []string{"instance"}, pattern.Captures)
		// labels.2 because: labels.0=com, labels.1=example, labels.2=<subdomain>
		assert.Equal(t, "{http.request.host.labels.2}", pattern.CaddyLabels["instance"])
	})

	t.Run("ParsePattern_DeepSubdomain", func(t *testing.T) {
		match := IngressMatch{Hostname: "{instance}.app.example.com"}
		pattern, err := match.ParsePattern()
		require.NoError(t, err)

		assert.Equal(t, "*.app.example.com", pattern.Wildcard)
		// labels.3 because: labels.0=com, labels.1=example, labels.2=app, labels.3=<subdomain>
		assert.Equal(t, "{http.request.host.labels.3}", pattern.CaddyLabels["instance"])
	})

	t.Run("ParsePattern_MultipleCaptures", func(t *testing.T) {
		match := IngressMatch{Hostname: "{instance}.{env}.example.com"}
		pattern, err := match.ParsePattern()
		require.NoError(t, err)

		assert.Equal(t, "*.*.example.com", pattern.Wildcard)
		assert.Equal(t, []string{"instance", "env"}, pattern.Captures)
		// {instance} at position 0 (from left) = labels.3
		// {env} at position 1 (from left) = labels.2
		assert.Equal(t, "{http.request.host.labels.3}", pattern.CaddyLabels["instance"])
		assert.Equal(t, "{http.request.host.labels.2}", pattern.CaddyLabels["env"])
	})

	t.Run("ParsePattern_NotAPattern", func(t *testing.T) {
		match := IngressMatch{Hostname: "api.example.com"}
		_, err := match.ParsePattern()
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "not a pattern")
	})

	t.Run("ParsePattern_MixedCapture", func(t *testing.T) {
		// Mixed captures like "api-{instance}" are not supported
		match := IngressMatch{Hostname: "api-{instance}.example.com"}
		_, err := match.ParsePattern()
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "mixed captures")
	})

	t.Run("ParsePattern_DuplicateCapture", func(t *testing.T) {
		match := IngressMatch{Hostname: "{instance}.{instance}.example.com"}
		_, err := match.ParsePattern()
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "duplicate capture")
	})

	t.Run("ResolveInstance", func(t *testing.T) {
		match := IngressMatch{Hostname: "{instance}.example.com"}
		pattern, err := match.ParsePattern()
		require.NoError(t, err)

		// Pattern reference should be resolved
		result := pattern.ResolveInstance("{instance}")
		assert.Equal(t, "{http.request.host.labels.2}", result)

		// Literal should remain unchanged
		result = pattern.ResolveInstance("my-api")
		assert.Equal(t, "my-api", result)
	})
}

// TestValidation_PatternHostnames tests validation of pattern-based ingress rules.
func TestValidation_PatternHostnames(t *testing.T) {
	t.Run("ValidPattern", func(t *testing.T) {
		req := CreateIngressRequest{
			Name: "pattern-ingress",
			Rules: []IngressRule{
				{
					Match:  IngressMatch{Hostname: "{instance}.example.com"},
					Target: IngressTarget{Instance: "{instance}", Port: 8080},
				},
			},
		}
		err := req.Validate()
		assert.NoError(t, err)
	})

	t.Run("PatternWithLiteralTarget", func(t *testing.T) {
		// Pattern hostname requires target.instance to reference a capture
		req := CreateIngressRequest{
			Name: "invalid-ingress",
			Rules: []IngressRule{
				{
					Match:  IngressMatch{Hostname: "{instance}.example.com"},
					Target: IngressTarget{Instance: "my-api", Port: 8080}, // Not a capture reference
				},
			},
		}
		err := req.Validate()
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "reference a capture")
	})

	t.Run("PatternWithUnknownCapture", func(t *testing.T) {
		// Target references a capture that doesn't exist in hostname
		req := CreateIngressRequest{
			Name: "invalid-ingress",
			Rules: []IngressRule{
				{
					Match:  IngressMatch{Hostname: "{instance}.example.com"},
					Target: IngressTarget{Instance: "{app}", Port: 8080}, // {app} not in hostname
				},
			},
		}
		err := req.Validate()
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "unknown capture")
	})

	t.Run("RawWildcardNotAllowed", func(t *testing.T) {
		req := CreateIngressRequest{
			Name: "wildcard-ingress",
			Rules: []IngressRule{
				{
					Match:  IngressMatch{Hostname: "*.example.com"},
					Target: IngressTarget{Instance: "my-api", Port: 8080},
				},
			},
		}
		err := req.Validate()
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "wildcard hostnames are not supported")
	})
}

// getFreePort returns a random available port.
func getFreePort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()
	return port
}

// TestConfigGeneration tests that config generation produces valid Caddy JSON.
func TestConfigGeneration(t *testing.T) {
	// Create temp dir
	tmpDir, err := os.MkdirTemp("", "ingress-validation-test-*")
	require.NoError(t, err)
	defer os.RemoveAll(tmpDir)

	p := paths.New(tmpDir)

	// Create required directories
	require.NoError(t, os.MkdirAll(p.CaddyDir(), 0755))
	require.NoError(t, os.MkdirAll(p.CaddyDataDir(), 0755))

	// Use random port to avoid test collisions
	adminPort := getFreePort(t)

	// Create config generator with DNS-based dynamic upstream settings
	dnsResolverPort := 5353
	generator := NewCaddyConfigGenerator(p, "0.0.0.0", "127.0.0.1", adminPort, ACMEConfig{}, dnsResolverPort)

	ctx := context.Background()

	t.Run("ValidConfig", func(t *testing.T) {
		// Create a valid ingress configuration
		ingresses := []Ingress{
			{
				ID:   "test-ingress-1",
				Name: "test-ingress",
				Rules: []IngressRule{
					{
						Match: IngressMatch{
							Hostname: "test.example.com",
							Port:     8080,
						},
						Target: IngressTarget{
							Instance: "test-instance",
							Port:     80,
						},
					},
				},
			},
		}

		// GenerateConfig should succeed and produce valid JSON
		data, err := generator.GenerateConfig(ctx, ingresses)
		require.NoError(t, err, "Valid config should generate successfully")

		// Verify it's valid JSON
		var config map[string]interface{}
		err = json.Unmarshal(data, &config)
		require.NoError(t, err, "Generated config should be valid JSON")

		// Verify essential structure
		assert.Contains(t, config, "admin")
		assert.Contains(t, config, "apps")

		// Verify DNS-based dynamic upstream is configured
		configStr := string(data)
		assert.Contains(t, configStr, "dynamic_upstreams")
		assert.Contains(t, configStr, "hypeman.internal")
	})

	t.Run("EmptyConfig", func(t *testing.T) {
		// Empty config should also be valid
		ingresses := []Ingress{}

		data, err := generator.GenerateConfig(ctx, ingresses)
		require.NoError(t, err, "Empty config should generate successfully")

		var config map[string]interface{}
		err = json.Unmarshal(data, &config)
		require.NoError(t, err, "Generated config should be valid JSON")
	})

	t.Run("MultipleRules", func(t *testing.T) {
		// Multiple rules with different ports
		ingresses := []Ingress{
			{
				ID:   "multi-ingress",
				Name: "multi-ingress",
				Rules: []IngressRule{
					{
						Match:  IngressMatch{Hostname: "api.example.com", Port: 80},
						Target: IngressTarget{Instance: "api-server", Port: 8080},
					},
					{
						Match:  IngressMatch{Hostname: "web.example.com", Port: 80},
						Target: IngressTarget{Instance: "web-server", Port: 3000},
					},
					{
						Match:  IngressMatch{Hostname: "admin.example.com", Port: 8443},
						Target: IngressTarget{Instance: "admin-server", Port: 9000},
					},
				},
			},
		}

		data, err := generator.GenerateConfig(ctx, ingresses)
		require.NoError(t, err, "Config with multiple rules should generate successfully")

		var config map[string]interface{}
		err = json.Unmarshal(data, &config)
		require.NoError(t, err, "Generated config should be valid JSON")

		// Verify routes are present
		configStr := string(data)
		assert.Contains(t, configStr, "api.example.com")
		assert.Contains(t, configStr, "web.example.com")
		assert.Contains(t, configStr, "admin.example.com")
	})

	t.Run("WriteConfig", func(t *testing.T) {
		ingresses := []Ingress{
			{
				ID:   "write-test",
				Name: "write-test",
				Rules: []IngressRule{
					{
						Match:  IngressMatch{Hostname: "test.example.com", Port: 80},
						Target: IngressTarget{Instance: "test", Port: 8080},
					},
				},
			},
		}

		err := generator.WriteConfig(ctx, ingresses)
		require.NoError(t, err, "WriteConfig should succeed")

		// Verify file was written
		assert.FileExists(t, p.CaddyConfig(), "Config file should be written")

		// Verify file content is valid JSON
		data, err := os.ReadFile(p.CaddyConfig())
		require.NoError(t, err)

		var config map[string]interface{}
		err = json.Unmarshal(data, &config)
		require.NoError(t, err, "Written config should be valid JSON")
	})

	t.Run("PatternHostname", func(t *testing.T) {
		// Test pattern-based hostname routing
		ingresses := []Ingress{
			{
				ID:   "pattern-ingress",
				Name: "pattern-ingress",
				Rules: []IngressRule{
					{
						Match:  IngressMatch{Hostname: "{instance}.example.com", Port: 80},
						Target: IngressTarget{Instance: "{instance}", Port: 8080},
					},
				},
			},
		}

		data, err := generator.GenerateConfig(ctx, ingresses)
		require.NoError(t, err, "Pattern config should generate successfully")

		configStr := string(data)

		// Verify wildcard is used for matching
		assert.Contains(t, configStr, "*.example.com")

		// Verify dynamic upstream uses Caddy placeholder for instance
		assert.Contains(t, configStr, "http.request.host.labels")
	})
}

// TestTLSConfigGeneration tests TLS-specific config generation.
func TestTLSConfigGeneration(t *testing.T) {
	// Create temp dir
	tmpDir, err := os.MkdirTemp("", "ingress-tls-validation-test-*")
	require.NoError(t, err)
	defer os.RemoveAll(tmpDir)

	p := paths.New(tmpDir)

	require.NoError(t, os.MkdirAll(p.CaddyDir(), 0755))
	require.NoError(t, os.MkdirAll(p.CaddyDataDir(), 0755))

	adminPort := getFreePort(t)
	dnsResolverPort := 5353

	t.Run("TLSWithCloudflare", func(t *testing.T) {
		acmeConfig := ACMEConfig{
			Email:              "admin@example.com",
			DNSProvider:        DNSProviderCloudflare,
			CloudflareAPIToken: "test-token",
		}
		generator := NewCaddyConfigGenerator(p, "0.0.0.0", "127.0.0.1", adminPort, acmeConfig, dnsResolverPort)

		ingresses := []Ingress{
			{
				ID:   "tls-ingress",
				Name: "tls-ingress",
				Rules: []IngressRule{
					{
						Match:        IngressMatch{Hostname: "secure.example.com", Port: 443},
						Target:       IngressTarget{Instance: "secure-app", Port: 8080},
						TLS:          true,
						RedirectHTTP: true,
					},
				},
			},
		}

		ctx := context.Background()

		data, err := generator.GenerateConfig(ctx, ingresses)
		require.NoError(t, err)

		configStr := string(data)

		// Verify TLS automation is configured
		assert.Contains(t, configStr, "automation")
		assert.Contains(t, configStr, "secure.example.com")
		assert.Contains(t, configStr, "cloudflare")
		assert.Contains(t, configStr, "admin@example.com")

		// Verify redirect is configured
		assert.Contains(t, configStr, "301")
		assert.Contains(t, configStr, "Location")
	})

	t.Run("NoTLSAutomationWithoutConfig", func(t *testing.T) {
		// Empty ACME config
		generator := NewCaddyConfigGenerator(p, "0.0.0.0", "127.0.0.1", adminPort, ACMEConfig{}, dnsResolverPort)

		ingresses := []Ingress{
			{
				ID:   "no-tls-ingress",
				Name: "no-tls-ingress",
				Rules: []IngressRule{
					{
						Match:  IngressMatch{Hostname: "test.example.com", Port: 80},
						Target: IngressTarget{Instance: "app", Port: 8080},
					},
				},
			},
		}

		ctx := context.Background()

		data, err := generator.GenerateConfig(ctx, ingresses)
		require.NoError(t, err)

		configStr := string(data)

		// Should NOT have TLS automation when ACME not configured
		assert.NotContains(t, configStr, `"automation"`)
	})
}
