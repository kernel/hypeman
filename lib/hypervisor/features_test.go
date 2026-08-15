package hypervisor

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

func fullCapabilities() Capabilities {
	return Capabilities{
		SupportsSnapshot:       true,
		SupportsFork:           true,
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

	t.Run("pause without snapshot yields pause only", func(t *testing.T) {
		t.Parallel()
		ids := Capabilities{SupportsPause: true}.FeatureIDs()
		require.Equal(t, []string{FeaturePause}, ids)
	})

	t.Run("snapshot support alone must not advertise fork", func(t *testing.T) {
		t.Parallel()
		// A snapshot-capable backend may still reject PrepareFork with
		// ErrNotSupported, so fork is an explicit capability — never inferred
		// from snapshots.
		ids := Capabilities{SupportsSnapshot: true}.FeatureIDs()
		require.Equal(t, []string{FeatureSnapshots}, ids)
	})

	t.Run("explicit fork support yields the fork ID", func(t *testing.T) {
		t.Parallel()
		ids := Capabilities{SupportsSnapshot: true, SupportsFork: true}.FeatureIDs()
		require.Equal(t, []string{FeatureSnapshots, FeatureFork}, ids)
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

// staticRegistration wraps a fixed capability set for enumeration tests.
func staticRegistration(caps Capabilities) RuntimeRegistration {
	return RuntimeRegistration{Capabilities: func() Capabilities { return caps }}
}

// TestEnumerateRuntimes exercises registry enumeration semantics against a
// local map so the global registry is never mutated by tests.
func TestEnumerateRuntimes(t *testing.T) {
	t.Parallel()

	t.Run("empty registry yields an empty non-nil list", func(t *testing.T) {
		t.Parallel()
		runtimes := enumerateRuntimes(map[Type]RuntimeRegistration{})
		require.NotNil(t, runtimes)
		require.Empty(t, runtimes)
	})

	t.Run("entries are sorted by type name for deterministic output", func(t *testing.T) {
		t.Parallel()
		byType := map[Type]RuntimeRegistration{
			TypeQEMU:            staticRegistration(Capabilities{SupportsPause: true}),
			TypeCloudHypervisor: staticRegistration(Capabilities{SupportsSnapshot: true}),
			TypeQEMUMicroVM:     staticRegistration(Capabilities{}),
			TypeFirecracker:     staticRegistration(Capabilities{SupportsVsock: true}),
		}
		runtimes := enumerateRuntimes(byType)
		require.Equal(t, []Type{TypeCloudHypervisor, TypeFirecracker, TypeQEMU, TypeQEMUMicroVM},
			[]Type{runtimes[0].Type, runtimes[1].Type, runtimes[2].Type, runtimes[3].Type})
		require.True(t, runtimes[0].Capabilities.SupportsSnapshot)
		require.True(t, runtimes[1].Capabilities.SupportsVsock)
		require.True(t, runtimes[2].Capabilities.SupportsPause)
	})

	t.Run("capabilities are resolved on every enumeration, not frozen", func(t *testing.T) {
		t.Parallel()
		// Mirrors config applied after init, e.g. a pinned cloud-hypervisor
		// default version changing the effective capability set.
		effective := Capabilities{SupportsDiskResize: true}
		byType := map[Type]RuntimeRegistration{
			TypeCloudHypervisor: {Capabilities: func() Capabilities { return effective }},
		}
		require.True(t, enumerateRuntimes(byType)[0].Capabilities.SupportsDiskResize)
		effective.SupportsDiskResize = false
		require.False(t, enumerateRuntimes(byType)[0].Capabilities.SupportsDiskResize,
			"enumeration must reflect the provider's current value")
	})

	t.Run("launch checks gate availability", func(t *testing.T) {
		t.Parallel()
		launchErr := errors.New("qemu-system-x86_64 not found")
		byType := map[Type]RuntimeRegistration{
			TypeCloudHypervisor: staticRegistration(Capabilities{}),
			TypeQEMU: {
				Capabilities: func() Capabilities { return Capabilities{} },
				LaunchCheck:  func() error { return launchErr },
			},
		}
		runtimes := enumerateRuntimes(byType)
		require.True(t, runtimes[0].Available(), "nil LaunchCheck means registration implies launchability")
		require.NoError(t, runtimes[0].LaunchErr)
		require.False(t, runtimes[1].Available(), "a failing launch check must mark the runtime unavailable")
		require.ErrorIs(t, runtimes[1].LaunchErr, launchErr)
	})

	t.Run("launch checks are re-evaluated on every enumeration", func(t *testing.T) {
		t.Parallel()
		// Installing the missing binary must flip availability without a
		// server restart.
		var launchErr error = errors.New("binary missing")
		byType := map[Type]RuntimeRegistration{
			TypeQEMU: {
				Capabilities: func() Capabilities { return Capabilities{} },
				LaunchCheck:  func() error { return launchErr },
			},
		}
		require.False(t, enumerateRuntimes(byType)[0].Available())
		launchErr = nil
		require.True(t, enumerateRuntimes(byType)[0].Available())
	})

	t.Run("results are value copies of the registry", func(t *testing.T) {
		t.Parallel()
		byType := map[Type]RuntimeRegistration{TypeQEMU: staticRegistration(Capabilities{SupportsPause: true})}
		runtimes := enumerateRuntimes(byType)
		runtimes[0].Capabilities.SupportsPause = false
		require.True(t, byType[TypeQEMU].Capabilities().SupportsPause, "mutating enumeration output must not affect the registry")
	})
}
