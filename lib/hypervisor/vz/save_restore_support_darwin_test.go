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
	require.True(t, capabilities().SupportsFork,
		"stopped-source forks clone disks without machine-state save/restore, so macOS 13 must still advertise fork")
	require.False(t, capabilities().SupportsStandby(),
		"macOS 13 must not advertise standby, so hot-source forks (which require standby) are not promised")

	macOSProductVersion = func() string { return "14.5" }
	require.Equal(t, saveRestoreSupported(), capabilities().SupportsSnapshot)
	require.True(t, capabilities().SupportsFork, "vz PrepareFork is implemented for every source state")

	macOSProductVersion = func() string { return "" }
	require.False(t, capabilities().SupportsSnapshot, "failed version probe must not advertise snapshot support")
	require.True(t, capabilities().SupportsFork,
		"fork does not depend on the save/restore probe: stopped-source forks work regardless")
}
