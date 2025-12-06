package ingress

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"testing"

	"github.com/onkernel/hypeman/lib/paths"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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

	// Create config generator
	generator := NewCaddyConfigGenerator(p, "0.0.0.0", "127.0.0.1", adminPort, ACMEConfig{})

	ctx := context.Background()
	ipResolver := func(instance string) (string, error) {
		return "10.100.0.10", nil
	}

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
		data, err := generator.GenerateConfig(ctx, ingresses, ipResolver)
		require.NoError(t, err, "Valid config should generate successfully")

		// Verify it's valid JSON
		var config map[string]interface{}
		err = json.Unmarshal(data, &config)
		require.NoError(t, err, "Generated config should be valid JSON")

		// Verify essential structure
		assert.Contains(t, config, "admin")
		assert.Contains(t, config, "apps")
	})

	t.Run("EmptyConfig", func(t *testing.T) {
		// Empty config should also be valid
		ingresses := []Ingress{}

		data, err := generator.GenerateConfig(ctx, ingresses, ipResolver)
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

		data, err := generator.GenerateConfig(ctx, ingresses, ipResolver)
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

		err := generator.WriteConfig(ctx, ingresses, ipResolver)
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

	t.Run("TLSWithCloudflare", func(t *testing.T) {
		acmeConfig := ACMEConfig{
			Email:              "admin@example.com",
			DNSProvider:        "cloudflare",
			CloudflareAPIToken: "test-token",
		}
		generator := NewCaddyConfigGenerator(p, "0.0.0.0", "127.0.0.1", adminPort, acmeConfig)

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
		ipResolver := func(instance string) (string, error) {
			return "10.100.0.10", nil
		}

		data, err := generator.GenerateConfig(ctx, ingresses, ipResolver)
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

	t.Run("TLSWithRoute53", func(t *testing.T) {
		acmeConfig := ACMEConfig{
			Email:              "admin@example.com",
			DNSProvider:        "route53",
			AWSAccessKeyID:     "AKID",
			AWSSecretAccessKey: "secret",
			AWSRegion:          "us-west-2",
		}
		generator := NewCaddyConfigGenerator(p, "0.0.0.0", "127.0.0.1", adminPort, acmeConfig)

		ingresses := []Ingress{
			{
				ID:   "tls-ingress",
				Name: "tls-ingress",
				Rules: []IngressRule{
					{
						Match:  IngressMatch{Hostname: "secure.example.com", Port: 443},
						Target: IngressTarget{Instance: "secure-app", Port: 8080},
						TLS:    true,
					},
				},
			},
		}

		ctx := context.Background()
		ipResolver := func(instance string) (string, error) {
			return "10.100.0.10", nil
		}

		data, err := generator.GenerateConfig(ctx, ingresses, ipResolver)
		require.NoError(t, err)

		configStr := string(data)

		// Verify Route53 is configured
		assert.Contains(t, configStr, "route53")
		assert.Contains(t, configStr, "AKID")
		assert.Contains(t, configStr, "us-west-2")
	})

	t.Run("NoTLSAutomationWithoutConfig", func(t *testing.T) {
		// Empty ACME config
		generator := NewCaddyConfigGenerator(p, "0.0.0.0", "127.0.0.1", adminPort, ACMEConfig{})

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
		ipResolver := func(instance string) (string, error) {
			return "10.100.0.10", nil
		}

		data, err := generator.GenerateConfig(ctx, ingresses, ipResolver)
		require.NoError(t, err)

		configStr := string(data)

		// Should NOT have TLS automation when ACME not configured
		assert.NotContains(t, configStr, `"automation"`)
	})
}
