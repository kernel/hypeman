//go:build darwin && arm64

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Code-Hex/vz/v3"
	"github.com/kernel/hypeman/lib/hypervisor/vz/shimconfig"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRosettaConfigureDirectorySharingDisabled(t *testing.T) {
	// When Rosetta is not requested the call is a no-op and never touches the
	// VM configuration, so a nil config is safe.
	err := configureDirectorySharing(nil, &shimconfig.ShimConfig{EnableRosetta: false})
	assert.NoError(t, err)
}

// TestRosettaConfigureDirectorySharingEnabled exercises the error paths
// unconditionally. The attach+validate path requires Rosetta to be installed on
// the runner; it is covered by TestRosettaConfigureDirectorySharingInstalled,
// which skips (visibly, under -v) when Rosetta is unavailable rather than
// silently asserting nothing.
func TestRosettaConfigureDirectorySharingEnabled(t *testing.T) {
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
	}
}

// TestRosettaConfigureDirectorySharingInstalled asserts that, when Rosetta is
// installed, configureDirectorySharing attaches the virtio-fs share and the
// resulting VM configuration validates. It skips otherwise so the log records
// whether the attach path actually ran on this runner.
func TestRosettaConfigureDirectorySharingInstalled(t *testing.T) {
	if vz.LinuxRosettaDirectoryShareAvailability() != vz.LinuxRosettaAvailabilityInstalled {
		t.Skip("rosetta not installed on this host; skipping attach+validate coverage")
	}

	vmConfig := newTestVMConfiguration(t)
	require.NoError(t, configureDirectorySharing(vmConfig, &shimconfig.ShimConfig{EnableRosetta: true}))
	ok, err := vmConfig.Validate()
	if err != nil && strings.Contains(err.Error(), "entitlement") {
		if os.Getenv("HYPEMAN_VZ_SIGNED") != "" {
			t.Fatalf("test binary should be signed with the virtualization entitlement (run via `make test-vz-shim-signed`): %v", err)
		}
		t.Skip("unsigned test binary lacks the virtualization entitlement; run via `make test-vz-shim-signed`")
	}
	require.NoError(t, err)
	assert.True(t, ok)
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
