package hypervisor

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func fullCapabilities() Capabilities {
	return Capabilities{
		SupportsSnapshot:       true,
		SupportsHotplugMemory:  true,
		SupportsBalloonControl: true,
		SupportsPause:          true,
		SupportsVsock:          true,
		SupportsGPUPassthrough: true,
		SupportsDiskIOLimit:    true,
		SupportsDiskResize:     true,
	}
}

func TestFeatureIDs(t *testing.T) {
	t.Parallel()

	t.Run("zero capabilities yield an empty non-nil list", func(t *testing.T) {
		t.Parallel()
		ids := Capabilities{}.FeatureIDs()
		require.NotNil(t, ids, "must serialize as [], not null")
		require.Empty(t, ids)
	})

	t.Run("full capabilities yield every client-visible ID in fixed order", func(t *testing.T) {
		t.Parallel()
		require.Equal(t, []string{
			FeatureSnapshots,
			FeatureStandby,
			FeatureFork,
			FeaturePause,
			FeatureHotplugMemory,
			FeatureBalloonControl,
			FeatureVsock,
			FeatureGPUPassthrough,
			FeatureDiskIOLimit,
			FeatureDiskResize,
		}, fullCapabilities().FeatureIDs())
	})

	t.Run("internal lifecycle hints have no feature IDs", func(t *testing.T) {
		t.Parallel()
		ids := Capabilities{
			SupportsGracefulVMMShutdown:       true,
			SupportsSnapshotBaseReuse:         true,
			RequiresHostSnapshotVersion:       true,
			SupportsConcurrentForkPrepare:     true,
			UsesDetachableSnapshotMemoryPager: true,
		}.FeatureIDs()
		require.Empty(t, ids)
	})

	t.Run("snapshot without pause yields snapshots and fork but not standby", func(t *testing.T) {
		t.Parallel()
		ids := Capabilities{SupportsSnapshot: true}.FeatureIDs()
		require.Equal(t, []string{FeatureSnapshots, FeatureFork}, ids)
	})

	t.Run("pause without snapshot yields pause only", func(t *testing.T) {
		t.Parallel()
		ids := Capabilities{SupportsPause: true}.FeatureIDs()
		require.Equal(t, []string{FeaturePause}, ids)
	})
}

// TestStandbySemantics pins that standby requires both snapshot and pause: a
// standby transition pauses the VM and then snapshots its memory.
func TestStandbySemantics(t *testing.T) {
	t.Parallel()
	require.False(t, Capabilities{SupportsSnapshot: true}.SupportsStandby())
	require.False(t, Capabilities{SupportsPause: true}.SupportsStandby())
	require.True(t, Capabilities{SupportsSnapshot: true, SupportsPause: true}.SupportsStandby())
}

// TestForkSemantics pins that fork tracks snapshot support: forking restores
// a snapshot of the source instance into the new VM.
func TestForkSemantics(t *testing.T) {
	t.Parallel()
	require.False(t, Capabilities{}.SupportsFork())
	require.False(t, Capabilities{SupportsPause: true}.SupportsFork())
	require.True(t, Capabilities{SupportsSnapshot: true}.SupportsFork())
}

// TestEnumerateRuntimes exercises registry enumeration semantics against a
// local map so the global registry is never mutated by tests.
func TestEnumerateRuntimes(t *testing.T) {
	t.Parallel()

	t.Run("empty registry yields an empty non-nil list", func(t *testing.T) {
		t.Parallel()
		runtimes := enumerateRuntimes(map[Type]Capabilities{})
		require.NotNil(t, runtimes)
		require.Empty(t, runtimes)
	})

	t.Run("entries are sorted by type name for deterministic output", func(t *testing.T) {
		t.Parallel()
		byType := map[Type]Capabilities{
			TypeQEMU:            {SupportsPause: true},
			TypeCloudHypervisor: {SupportsSnapshot: true},
			TypeQEMUMicroVM:     {},
			TypeFirecracker:     {SupportsVsock: true},
		}
		runtimes := enumerateRuntimes(byType)
		require.Equal(t, []Type{TypeCloudHypervisor, TypeFirecracker, TypeQEMU, TypeQEMUMicroVM},
			[]Type{runtimes[0].Type, runtimes[1].Type, runtimes[2].Type, runtimes[3].Type})
		require.True(t, runtimes[0].Capabilities.SupportsSnapshot)
		require.True(t, runtimes[1].Capabilities.SupportsVsock)
		require.True(t, runtimes[2].Capabilities.SupportsPause)
	})

	t.Run("results are value copies of the registry", func(t *testing.T) {
		t.Parallel()
		byType := map[Type]Capabilities{TypeQEMU: {SupportsPause: true}}
		runtimes := enumerateRuntimes(byType)
		runtimes[0].Capabilities.SupportsPause = false
		require.True(t, byType[TypeQEMU].SupportsPause, "mutating enumeration output must not affect the registry")
	})
}
