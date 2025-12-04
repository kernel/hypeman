package ingress

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/onkernel/hypeman/lib/paths"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

func setupTestGenerator(t *testing.T) (*EnvoyConfigGenerator, *paths.Paths, func()) {
	t.Helper()

	// Create temp dir
	tmpDir, err := os.MkdirTemp("", "ingress-config-test-*")
	require.NoError(t, err)

	p := paths.New(tmpDir)

	// Create required directories
	require.NoError(t, os.MkdirAll(p.EnvoyDir(), 0755))

	// Pass nil for validator in tests - no real Envoy binary available
	// Empty OTELConfig means OTEL is disabled
	generator := NewEnvoyConfigGenerator(p, "0.0.0.0", "127.0.0.1", 9901, nil, OTELConfig{})

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
	ipResolver := func(instance string) (string, error) {
		return "10.100.0.10", nil
	}

	data, err := generator.GenerateConfig(ctx, ingresses, ipResolver)
	require.NoError(t, err)

	// Parse YAML to verify structure
	var config map[string]interface{}
	err = yaml.Unmarshal(data, &config)
	require.NoError(t, err)

	// Should have admin section
	admin, ok := config["admin"].(map[string]interface{})
	require.True(t, ok, "config should have admin section")
	adminAddr := admin["address"].(map[string]interface{})
	socketAddr := adminAddr["socket_address"].(map[string]interface{})
	assert.Equal(t, "127.0.0.1", socketAddr["address"])
	assert.Equal(t, 9901, socketAddr["port_value"])

	// Should have empty listeners and clusters
	staticResources := config["static_resources"].(map[string]interface{})
	listeners := staticResources["listeners"].([]interface{})
	assert.Empty(t, listeners, "listeners should be empty for no ingresses")
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
	ipResolver := func(instance string) (string, error) {
		if instance == "my-api" {
			return "10.100.0.10", nil
		}
		return "", ErrInstanceNotFound
	}

	data, err := generator.GenerateConfig(ctx, ingresses, ipResolver)
	require.NoError(t, err)

	configStr := string(data)

	// Verify key elements are present
	assert.Contains(t, configStr, "api.example.com", "config should contain hostname")
	assert.Contains(t, configStr, "10.100.0.10", "config should contain instance IP")
	assert.Contains(t, configStr, "8080", "config should contain port")
	assert.Contains(t, configStr, "ingress_ing-123", "config should contain cluster name")
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

	ipResolver := func(instance string) (string, error) {
		switch instance {
		case "api-service":
			return "10.100.0.10", nil
		case "web-service":
			return "10.100.0.11", nil
		}
		return "", ErrInstanceNotFound
	}

	data, err := generator.GenerateConfig(ctx, ingresses, ipResolver)
	require.NoError(t, err)

	configStr := string(data)

	// Verify both hosts are present
	assert.Contains(t, configStr, "api.example.com")
	assert.Contains(t, configStr, "web.example.com")
	assert.Contains(t, configStr, "10.100.0.10")
	assert.Contains(t, configStr, "10.100.0.11")
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

	ipResolver := func(instance string) (string, error) {
		switch instance {
		case "app1":
			return "10.100.0.10", nil
		case "app2":
			return "10.100.0.20", nil
		}
		return "", ErrInstanceNotFound
	}

	data, err := generator.GenerateConfig(ctx, ingresses, ipResolver)
	require.NoError(t, err)

	configStr := string(data)

	// Verify all hosts and IPs are present
	assert.Contains(t, configStr, "app1.example.com")
	assert.Contains(t, configStr, "app2.example.com")
	assert.Contains(t, configStr, "10.100.0.10")
	assert.Contains(t, configStr, "10.100.0.20")
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

	ipResolver := func(instance string) (string, error) {
		switch instance {
		case "api":
			return "10.100.0.10", nil
		case "internal":
			return "10.100.0.20", nil
		case "metrics":
			return "10.100.0.30", nil
		}
		return "", ErrInstanceNotFound
	}

	data, err := generator.GenerateConfig(ctx, ingresses, ipResolver)
	require.NoError(t, err)

	configStr := string(data)

	// Verify listeners for each port
	assert.Contains(t, configStr, "ingress_listener_80")
	assert.Contains(t, configStr, "ingress_listener_8080")
	assert.Contains(t, configStr, "ingress_listener_9000")

	// Verify all hostnames are present
	assert.Contains(t, configStr, "api.example.com")
	assert.Contains(t, configStr, "internal.example.com")
	assert.Contains(t, configStr, "metrics.example.com")

	// Verify all IPs are present
	assert.Contains(t, configStr, "10.100.0.10")
	assert.Contains(t, configStr, "10.100.0.20")
	assert.Contains(t, configStr, "10.100.0.30")
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

	ipResolver := func(instance string) (string, error) {
		return "10.100.0.10", nil
	}

	data, err := generator.GenerateConfig(ctx, ingresses, ipResolver)
	require.NoError(t, err)

	configStr := string(data)

	// Should create listener on port 80 (default)
	assert.Contains(t, configStr, "ingress_listener_80")
	assert.Contains(t, configStr, "port_value: 80")
}

func TestGenerateConfig_SkipsUnresolvedInstances(t *testing.T) {
	generator, _, cleanup := setupTestGenerator(t)
	defer cleanup()

	ctx := context.Background()
	ingresses := []Ingress{
		{
			ID:   "ing-123",
			Name: "partial-ingress",
			Rules: []IngressRule{
				{
					Match:  IngressMatch{Hostname: "valid.example.com"},
					Target: IngressTarget{Instance: "valid-instance", Port: 8080},
				},
				{
					Match:  IngressMatch{Hostname: "invalid.example.com"},
					Target: IngressTarget{Instance: "missing-instance", Port: 8080},
				},
			},
		},
	}

	ipResolver := func(instance string) (string, error) {
		if instance == "valid-instance" {
			return "10.100.0.10", nil
		}
		return "", ErrInstanceNotFound
	}

	data, err := generator.GenerateConfig(ctx, ingresses, ipResolver)
	require.NoError(t, err)

	configStr := string(data)

	// Valid instance should be present
	assert.Contains(t, configStr, "valid.example.com")
	assert.Contains(t, configStr, "10.100.0.10")

	// Invalid instance should NOT be present
	assert.NotContains(t, configStr, "invalid.example.com")
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

	ipResolver := func(instance string) (string, error) {
		return "10.100.0.10", nil
	}

	err := generator.WriteConfig(ctx, ingresses, ipResolver)
	require.NoError(t, err)

	// Verify file was written
	configPath := p.EnvoyConfig()
	data, err := os.ReadFile(configPath)
	require.NoError(t, err)
	assert.True(t, len(data) > 0, "config file should not be empty")
	assert.Contains(t, string(data), "test.example.com")
}

func TestSanitizeHostname(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"api.example.com", "api_example_com"},
		{"my-service.domain.org", "my_service_domain_org"},
		{"simple", "simple"},
		{"a.b.c.d", "a_b_c_d"},
	}

	for _, tc := range tests {
		t.Run(tc.input, func(t *testing.T) {
			result := sanitizeHostname(tc.input)
			assert.Equal(t, tc.expected, result)
		})
	}
}

func TestConfigIsValidYAML(t *testing.T) {
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

	ipResolver := func(instance string) (string, error) {
		return "10.100.0.10", nil
	}

	data, err := generator.GenerateConfig(ctx, ingresses, ipResolver)
	require.NoError(t, err)

	// Verify it's valid YAML by parsing it
	var config interface{}
	err = yaml.Unmarshal(data, &config)
	require.NoError(t, err, "generated config should be valid YAML")

	// Also check that there are no obvious YAML issues (multiple documents, etc)
	assert.False(t, strings.Contains(string(data), "---\n"), "should be single YAML document")
}

func TestGenerateConfig_WithOTEL(t *testing.T) {
	// Create temp dir
	tmpDir, err := os.MkdirTemp("", "ingress-config-otel-test-*")
	require.NoError(t, err)
	defer os.RemoveAll(tmpDir)

	p := paths.New(tmpDir)
	require.NoError(t, os.MkdirAll(p.EnvoyDir(), 0755))

	// Create generator with OTEL enabled
	otelConfig := OTELConfig{
		Enabled:           true,
		Endpoint:          "otel-collector:4317",
		ServiceName:       "test-service",
		ServiceInstanceID: "instance-123",
		Insecure:          true,
		Environment:       "test",
	}
	generator := NewEnvoyConfigGenerator(p, "0.0.0.0", "127.0.0.1", 9901, nil, otelConfig)

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

	ipResolver := func(instance string) (string, error) {
		return "10.100.0.10", nil
	}

	data, err := generator.GenerateConfig(ctx, ingresses, ipResolver)
	require.NoError(t, err)

	configStr := string(data)

	// Verify OTEL collector cluster is present
	assert.Contains(t, configStr, "opentelemetry_collector", "config should contain OTEL collector cluster")
	assert.Contains(t, configStr, "otel-collector", "config should contain OTEL collector host")
	assert.Contains(t, configStr, "4317", "config should contain OTEL collector port")

	// Verify tracing config is present
	assert.Contains(t, configStr, "envoy.tracers.opentelemetry", "config should contain OTEL tracer")
	assert.Contains(t, configStr, "test-service", "config should contain service name")

	// Verify resource attributes are present
	assert.Contains(t, configStr, "deployment.environment.name", "config should contain environment attribute")
	assert.Contains(t, configStr, "service.instance.id", "config should contain instance ID attribute")
}

func TestGenerateConfig_WithOTELDisabled(t *testing.T) {
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

	ipResolver := func(instance string) (string, error) {
		return "10.100.0.10", nil
	}

	data, err := generator.GenerateConfig(ctx, ingresses, ipResolver)
	require.NoError(t, err)

	configStr := string(data)

	// Verify OTEL is NOT present when disabled
	assert.NotContains(t, configStr, "opentelemetry_collector", "config should not contain OTEL collector when disabled")
	assert.NotContains(t, configStr, "envoy.tracers.opentelemetry", "config should not contain OTEL tracer when disabled")
}
