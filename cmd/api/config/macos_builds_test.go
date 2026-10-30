package config

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestMacOSBuildsFailClosedConfiguration(t *testing.T) {
	require.False(t, (&Config{}).MacOSBuilds)
	c := *defaultConfig()
	c.MacOSBuilds = true
	require.ErrorContains(t, c.validateMacOSBuilds(), "requires macos_only")
	c.MacOSOnly = true
	c.Hypervisor.Default = "vz"
	c.Registry.URL = ""
	require.ErrorContains(t, c.validateMacOSBuilds(), "explicit registry URL")
	c.Registry.URL = "http://localhost:4973"
	c.Registry.CACertFile = "custom.pem"
	require.ErrorContains(t, c.validateMacOSBuilds(), "system-trusted registry TLS")
	c.Registry.CACertFile = ""
	c.Registry.Insecure = true
	require.ErrorContains(t, c.validateMacOSBuilds(), "system-trusted registry TLS")
	c.Registry.Insecure = false
	require.NoError(t, c.validateMacOSBuilds())
}
