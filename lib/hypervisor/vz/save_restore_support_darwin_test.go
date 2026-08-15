//go:build darwin

package vz

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestCapabilitiesDeriveSnapshotFromHost proves the hypervisor's advertised
// capabilities track the probed macOS version instead of a static arch check.
func TestCapabilitiesDeriveSnapshotFromHost(t *testing.T) {
	orig := macOSProductVersion
	t.Cleanup(func() { macOSProductVersion = orig })

	macOSProductVersion = func() string { return "13.6" }
	require.False(t, capabilities().SupportsSnapshot, "macOS 13 must not advertise snapshot/standby support")
	require.False(t, capabilities().SupportsFork, "fork restores a snapshot, so macOS 13 must not advertise it")

	macOSProductVersion = func() string { return "14.5" }
	require.Equal(t, saveRestoreSupported(), capabilities().SupportsSnapshot)
	require.Equal(t, capabilities().SupportsSnapshot, capabilities().SupportsFork,
		"vz PrepareFork rewrites snapshot manifests, so fork must track the same save/restore probe")

	macOSProductVersion = func() string { return "" }
	require.False(t, capabilities().SupportsSnapshot, "failed version probe must not advertise snapshot support")
	require.False(t, capabilities().SupportsFork, "failed version probe must not advertise fork support")
}
