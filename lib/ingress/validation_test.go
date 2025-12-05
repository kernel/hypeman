package ingress

import (
	"context"
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

// TestXDSValidation tests that xDS config validation works with the embedded Envoy binary.
// This test verifies that:
// - Valid LDS/CDS configs pass validation
// - Invalid LDS/CDS configs fail validation
func TestXDSValidation(t *testing.T) {
	// Create temp dir
	tmpDir, err := os.MkdirTemp("", "ingress-validation-test-*")
	require.NoError(t, err)
	defer os.RemoveAll(tmpDir)

	p := paths.New(tmpDir)

	// Create required directories
	require.NoError(t, os.MkdirAll(p.EnvoyDir(), 0755))

	// Extract the embedded Envoy binary
	envoyPath, err := ExtractEnvoyBinary(p)
	require.NoError(t, err, "Should be able to extract embedded Envoy binary")
	require.FileExists(t, envoyPath, "Envoy binary should exist after extraction")

	// Use random port to avoid test collisions
	adminPort := getFreePort(t)

	// Create daemon for validation (it has ValidateConfig method)
	daemon := NewEnvoyDaemon(p, "127.0.0.1", adminPort, true)

	// Create config generator with daemon as validator
	generator := NewEnvoyConfigGenerator(p, "0.0.0.0", "127.0.0.1", adminPort, daemon, OTELConfig{})

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

		// WriteConfig should succeed with valid config
		err := generator.WriteConfig(ctx, ingresses, ipResolver)
		if err != nil {
			t.Logf("WriteConfig error: %v", err)
			// Try to get more details by reading any written files
			if ldsData, readErr := os.ReadFile(p.EnvoyLDS()); readErr == nil {
				t.Logf("LDS content:\n%s", string(ldsData))
			}
			if cdsData, readErr := os.ReadFile(p.EnvoyCDS()); readErr == nil {
				t.Logf("CDS content:\n%s", string(cdsData))
			}
		}
		require.NoError(t, err, "Valid config should pass validation")

		// Verify files were written
		assert.FileExists(t, p.EnvoyLDS(), "LDS file should be written")
		assert.FileExists(t, p.EnvoyCDS(), "CDS file should be written")
		assert.FileExists(t, p.EnvoyConfig(), "Bootstrap file should be written")
	})

	t.Run("EmptyConfig", func(t *testing.T) {
		// Empty config should also be valid
		ingresses := []Ingress{}

		err := generator.WriteConfig(ctx, ingresses, ipResolver)
		require.NoError(t, err, "Empty config should pass validation")
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

		err := generator.WriteConfig(ctx, ingresses, ipResolver)
		require.NoError(t, err, "Config with multiple rules should pass validation")
	})
}

// TestXDSValidationWithInvalidConfig tests that invalid configs are rejected.
func TestXDSValidationWithInvalidConfig(t *testing.T) {
	// Create temp dir
	tmpDir, err := os.MkdirTemp("", "ingress-invalid-validation-test-*")
	require.NoError(t, err)
	defer os.RemoveAll(tmpDir)

	p := paths.New(tmpDir)

	// Create required directories
	require.NoError(t, os.MkdirAll(p.EnvoyDir(), 0755))

	// Extract the embedded Envoy binary
	_, err = ExtractEnvoyBinary(p)
	require.NoError(t, err)

	// Use random port to avoid test collisions
	adminPort := getFreePort(t)

	// Create daemon for validation
	daemon := NewEnvoyDaemon(p, "127.0.0.1", adminPort, true)

	t.Run("InvalidYAMLFormat", func(t *testing.T) {
		// Write invalid YAML to the bootstrap config path
		invalidConfig := `
admin:
  address:
    socket_address:
      address: "127.0.0.1"
      port_value: 9901
dynamic_resources:
  lds_config:
    path_config_source:
      path: "/nonexistent/lds.yaml"
  cds_config:
    path_config_source:
      path: "/nonexistent/cds.yaml"
`
		// Write to a temp file for validation
		tmpFile, err := os.CreateTemp(tmpDir, "invalid-*.yaml")
		require.NoError(t, err)
		defer os.Remove(tmpFile.Name())

		_, err = tmpFile.WriteString(invalidConfig)
		require.NoError(t, err)
		tmpFile.Close()

		// Validation should fail because referenced files don't exist
		err = daemon.ValidateConfig(tmpFile.Name())
		assert.Error(t, err, "Config with nonexistent xDS files should fail validation")
	})

	t.Run("MalformedYAML", func(t *testing.T) {
		// Write malformed YAML
		malformedConfig := `
admin:
  address:
    socket_address
      address: "127.0.0.1"
      port_value: this_should_be_int
`
		tmpFile, err := os.CreateTemp(tmpDir, "malformed-*.yaml")
		require.NoError(t, err)
		defer os.Remove(tmpFile.Name())

		_, err = tmpFile.WriteString(malformedConfig)
		require.NoError(t, err)
		tmpFile.Close()

		err = daemon.ValidateConfig(tmpFile.Name())
		assert.Error(t, err, "Malformed YAML should fail validation")
	})

	t.Run("InvalidListenerConfig", func(t *testing.T) {
		// Create config with invalid listener (negative port)
		ldsConfig := `
resources:
  - "@type": type.googleapis.com/envoy.config.listener.v3.Listener
    name: invalid_listener
    address:
      socket_address:
        address: "0.0.0.0"
        port_value: -1
`
		cdsConfig := `
resources: []
`
		// Write LDS
		ldsFile, err := os.CreateTemp(tmpDir, "invalid-lds-*.yaml")
		require.NoError(t, err)
		defer os.Remove(ldsFile.Name())
		_, err = ldsFile.WriteString(ldsConfig)
		require.NoError(t, err)
		ldsFile.Close()

		// Write CDS
		cdsFile, err := os.CreateTemp(tmpDir, "invalid-cds-*.yaml")
		require.NoError(t, err)
		defer os.Remove(cdsFile.Name())
		_, err = cdsFile.WriteString(cdsConfig)
		require.NoError(t, err)
		cdsFile.Close()

		// Write bootstrap referencing these files
		bootstrapConfig := `
admin:
  address:
    socket_address:
      address: "127.0.0.1"
      port_value: 9901
dynamic_resources:
  lds_config:
    path_config_source:
      path: "` + ldsFile.Name() + `"
      watched_directory:
        path: "` + tmpDir + `"
  cds_config:
    path_config_source:
      path: "` + cdsFile.Name() + `"
      watched_directory:
        path: "` + tmpDir + `"
`
		bootstrapFile, err := os.CreateTemp(tmpDir, "invalid-bootstrap-*.yaml")
		require.NoError(t, err)
		defer os.Remove(bootstrapFile.Name())
		_, err = bootstrapFile.WriteString(bootstrapConfig)
		require.NoError(t, err)
		bootstrapFile.Close()

		err = daemon.ValidateConfig(bootstrapFile.Name())
		assert.Error(t, err, "Config with invalid listener port should fail validation")
	})
}
