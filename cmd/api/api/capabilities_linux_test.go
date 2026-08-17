//go:build linux

package api

import (
	"runtime"
	"testing"

	"github.com/kernel/hypeman/lib/hypervisor"
	"github.com/stretchr/testify/require"
)

// TestRegisteredRuntimesLinux pins the Linux registration boundary: the
// capability registry contains exactly the runtimes launchable on this host,
// in deterministic sorted order. qemu-microvm appears only where its x86
// board exists, and vz never registers off macOS.
func TestRegisteredRuntimesLinux(t *testing.T) {
	t.Parallel()

	expected := []string{"cloud-hypervisor", "firecracker", "qemu"}
	if runtime.GOARCH == "amd64" {
		expected = append(expected, "qemu-microvm")
	}

	names := make([]string, 0, len(expected))
	for _, rt := range hypervisor.RegisteredRuntimes() {
		names = append(names, string(rt.Type))
	}
	require.Equal(t, expected, names)

	_, vzRegistered := hypervisor.CapabilitiesForType(hypervisor.TypeVZ)
	require.False(t, vzRegistered, "vz must not register capabilities on Linux")
}
