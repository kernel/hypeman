package config

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestMachineBuildStorageConfig(t *testing.T) {
	for _, mutate := range []func(*BuildConfig){
		func(b *BuildConfig) { b.MachineMaxConcurrent = 0 },
		func(b *BuildConfig) { b.MachineMaxExportBytes = 0 },
		func(b *BuildConfig) { b.MachineMinFreeBytes = -1 },
		func(b *BuildConfig) { b.MachineMaxExportBytes = 1<<63 - 1 },
		func(b *BuildConfig) { b.MachineMinFreeBytes = 1<<63 - 1 },
	} {
		c := defaultConfig()
		mutate(&c.Build)
		require.NoError(t, c.validateMacOSBuilds(), "disabled machine limits must not affect Linux")
		c.MacOSBuilds, c.MacOSOnly = true, true
		c.Registry.URL = "http://localhost:4973"
		require.ErrorContains(t, c.validateMacOSBuilds(), "storage limits")
	}
	c := defaultConfig()
	require.Equal(t, 1, c.Build.MachineMaxConcurrent)
	require.EqualValues(t, 128<<30, c.Build.MachineMaxExportBytes)
	require.EqualValues(t, 10<<30, c.Build.MachineMinFreeBytes)
}

func TestMacOSBuildsFailClosedConfiguration(t *testing.T) {
	require.False(t, (&Config{}).MacOSBuilds)
	c := *defaultConfig()
	c.MacOSBuilds = true
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
