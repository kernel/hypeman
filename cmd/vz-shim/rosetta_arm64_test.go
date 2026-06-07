//go:build darwin && arm64

package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/Code-Hex/vz/v3"
	"github.com/kernel/hypeman/lib/hypervisor/vz/shimconfig"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestConfigureDirectorySharingDisabled(t *testing.T) {
	// When Rosetta is not requested the call is a no-op and never touches the
	// VM configuration, so a nil config is safe.
	err := configureDirectorySharing(nil, &shimconfig.ShimConfig{EnableRosetta: false})
	assert.NoError(t, err)
}

func TestConfigureDirectorySharingEnabled(t *testing.T) {
	cfg := &shimconfig.ShimConfig{EnableRosetta: true}

	switch vz.LinuxRosettaDirectoryShareAvailability() {
	case vz.LinuxRosettaAvailabilityNotInstalled:
		err := configureDirectorySharing(nil, cfg)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "not installed")
	case vz.LinuxRosettaAvailabilityNotSupported:
		err := configureDirectorySharing(nil, cfg)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "not supported")
	case vz.LinuxRosettaAvailabilityInstalled:
		vmConfig := newTestVMConfiguration(t)
		require.NoError(t, configureDirectorySharing(vmConfig, cfg))
		ok, err := vmConfig.Validate()
		require.NoError(t, err)
		assert.True(t, ok)
	}
}

// newTestVMConfiguration builds a minimal valid VM configuration for tests that
// need to attach a device. The kernel file only needs to exist for the boot
// loader constructor.
func newTestVMConfiguration(t *testing.T) *vz.VirtualMachineConfiguration {
	t.Helper()

	kernelPath := filepath.Join(t.TempDir(), "vmlinuz")
	require.NoError(t, os.WriteFile(kernelPath, []byte("kernel"), 0o644))

	bootLoader, err := vz.NewLinuxBootLoader(kernelPath, vz.WithCommandLine("console=hvc0"))
	require.NoError(t, err)

	vmConfig, err := vz.NewVirtualMachineConfiguration(bootLoader, 1, 256*1024*1024)
	require.NoError(t, err)
	return vmConfig
}
