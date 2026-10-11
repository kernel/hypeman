package config

import (
	"github.com/stretchr/testify/require"
	"runtime"
	"testing"
)

func TestMacOSBuildsFailClosedConfiguration(t *testing.T) {
	if runtime.GOOS != "darwin" || runtime.GOARCH != "arm64" {
		t.Skip("macos_only requires Apple silicon")
	}
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
	c.Registry.URL = "http://127.0.0.1:4973"
	require.NoError(t, c.validateMacOSBuilds())
	c.Registry.URL = "http://localhost:4973"
	require.NoError(t, c.validateMacOSBuilds())
	c.Registry.URL = "http://[::1]"
	require.NoError(t, c.validateMacOSBuilds())
}
