package config

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestMacOSBuildsFailClosedConfiguration(t *testing.T) {
	require.False(t, (&Config{}).MacOSBuilds)
	c := Config{MacOSBuilds: true}
	require.ErrorContains(t, c.validateMacOSBuilds(), "requires macos_only")
	c.MacOSOnly = true
	c.Hypervisor.Default = "vz"
	c.Registry.CACertFile = "custom.pem"
	require.ErrorContains(t, c.validateMacOSBuilds(), "system-trusted registry TLS")
	c.Registry.CACertFile = ""
	c.Registry.URL = "https://registry.example"
	c.Registry.Insecure = true
	require.ErrorContains(t, c.validateMacOSBuilds(), "system-trusted registry TLS")
	c.Registry.Insecure = false
	c.Registry.URL = "http://registry.example:4973"
	require.ErrorContains(t, c.validateMacOSBuilds(), "must be loopback")
	c.Registry.URL = "localhost:4973"
	require.ErrorContains(t, c.validateMacOSBuilds(), "explicit registry URL")
	c.Registry.URL = "https://registry.example/prefix"
	require.ErrorContains(t, c.validateMacOSBuilds(), "without credentials")
	c.Registry.URL = "https://user:pass@registry.example"
	require.ErrorContains(t, c.validateMacOSBuilds(), "without credentials")
	c.Registry.URL = "https://registry.example?debug=1"
	require.ErrorContains(t, c.validateMacOSBuilds(), "without credentials")
	c.Registry.URL = "http://127.0.0.1:4973"
	require.NoError(t, c.validateMacOSBuilds())
	c.Registry.URL = "http://localhost:4973"
	require.NoError(t, c.validateMacOSBuilds())
	c.Registry.URL = "http://[::1]"
	require.NoError(t, c.validateMacOSBuilds())
}
