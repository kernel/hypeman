//go:build linux

package firecracker

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/kernel/hypeman/lib/hypervisor"
	"github.com/stretchr/testify/require"
)

// registeredFirecracker returns firecracker's capability-registry entry,
// re-resolved on every call so launch checks reflect the current
// configuration.
func registeredFirecracker(t *testing.T) hypervisor.RegisteredRuntime {
	t.Helper()
	for _, rt := range hypervisor.RegisteredRuntimes() {
		if rt.Type == hypervisor.TypeFirecracker {
			return rt
		}
	}
	t.Fatal("firecracker must be registered on Linux")
	return hypervisor.RegisteredRuntime{}
}

// TestFirecrackerAvailabilityTracksBinaryOverride pins that firecracker's
// registered availability follows the configured binary override, which
// takes precedence over the embedded binaries on every launch
// (resolveBinaryPath): an invalid hypervisor.firecracker_binary_path means
// every launch fails, so the runtime must not report available; clearing or
// fixing the override must flip availability back without re-registration.
// Not parallel: it mutates the process-wide custom binary path.
func TestFirecrackerAvailabilityTracksBinaryOverride(t *testing.T) {
	SetCustomBinaryPath("")
	t.Cleanup(func() { SetCustomBinaryPath("") })

	// No override: the embedded binary implies launchability.
	rt := registeredFirecracker(t)
	require.NoError(t, rt.LaunchErr)
	require.True(t, rt.Available(), "firecracker with embedded binaries must be available")

	// Missing override: launches would fail, so availability must drop.
	SetCustomBinaryPath("/does/not/exist/firecracker")
	rt = registeredFirecracker(t)
	require.Error(t, rt.LaunchErr)
	require.False(t, rt.Available(), "a missing binary override must make firecracker unavailable")
	require.Contains(t, rt.LaunchErr.Error(), "invalid firecracker custom binary path",
		"the launch check must surface the same override error launches report")

	// Non-executable override: same verdict as the launch path's check.
	nonExec := filepath.Join(t.TempDir(), "firecracker")
	require.NoError(t, os.WriteFile(nonExec, []byte("#!/bin/sh\nexit 0\n"), 0644))
	SetCustomBinaryPath(nonExec)
	rt = registeredFirecracker(t)
	require.Error(t, rt.LaunchErr)
	require.False(t, rt.Available(), "a non-executable binary override must make firecracker unavailable")

	// Valid executable override: available again, per registry read.
	valid := filepath.Join(t.TempDir(), "firecracker")
	require.NoError(t, os.WriteFile(valid, []byte("#!/bin/sh\nexit 0\n"), 0755))
	SetCustomBinaryPath(valid)
	rt = registeredFirecracker(t)
	require.NoError(t, rt.LaunchErr)
	require.True(t, rt.Available(), "a valid binary override must restore availability without a restart")
}
