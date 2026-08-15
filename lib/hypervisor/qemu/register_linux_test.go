//go:build linux

package qemu

import (
	"runtime"
	"testing"

	"github.com/kernel/hypeman/lib/hypervisor"
	"github.com/stretchr/testify/require"
)

// TestMicroVMRegistrationMatchesBoardSupport pins that qemu-microvm's presence
// in the capability registry tracks the same machine-type resolver that gates
// launches, so the registry never advertises a board the host cannot boot.
func TestMicroVMRegistrationMatchesBoardSupport(t *testing.T) {
	t.Parallel()

	_, qemuRegistered := hypervisor.CapabilitiesForType(hypervisor.TypeQEMU)
	require.True(t, qemuRegistered, "standard qemu must register on Linux")

	_, boardErr := resolveMachineTypeForPlatform(MachineTypeMicroVM, runtime.GOOS, runtime.GOARCH)
	_, microVMRegistered := hypervisor.CapabilitiesForType(hypervisor.TypeQEMUMicroVM)
	require.Equal(t, boardErr == nil, microVMRegistered,
		"qemu-microvm registration must match microvm board support on %s/%s", runtime.GOOS, runtime.GOARCH)
}

// TestQEMUAvailabilityTracksSystemBinary pins that registered QEMU runtimes
// report availability from the same binary lookup that launches use: hypeman
// deliberately starts without a system QEMU (cmd/api/main.go only warns), so
// registration alone must not imply launchability.
func TestQEMUAvailabilityTracksSystemBinary(t *testing.T) {
	t.Parallel()

	_, binErr := (&Starter{}).GetBinaryPath(nil, "")
	checked := 0
	for _, rt := range hypervisor.RegisteredRuntimes() {
		if rt.Type != hypervisor.TypeQEMU && rt.Type != hypervisor.TypeQEMUMicroVM {
			continue
		}
		checked++
		require.Equal(t, binErr == nil, rt.Available(),
			"%s availability must match the launch-path binary lookup", rt.Type)
		if binErr != nil {
			require.Error(t, rt.LaunchErr)
		} else {
			require.NoError(t, rt.LaunchErr)
		}
	}
	require.GreaterOrEqual(t, checked, 1, "standard qemu must be registered on Linux")
}
