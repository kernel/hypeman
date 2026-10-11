//go:build darwin && arm64

package instances

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kernel/hypeman/lib/hypervisor"
	"github.com/kernel/hypeman/lib/hypervisor/vz"
	"github.com/kernel/hypeman/lib/instances/phasetracking"
	"github.com/kernel/hypeman/lib/paths"
	"github.com/stretchr/testify/require"
)

func TestMacOSStoppedSnapshotForkRebindsIdentity(t *testing.T) {
	p := paths.New(t.TempDir())
	m := &manager{paths: p, macOSOnly: true, lifecycleEvents: newLifecycleSubscribers(), vmStarters: map[hypervisor.Type]hypervisor.VMStarter{hypervisor.TypeVZ: concurrentForkPrepareTestStarter{}}}
	require.NoError(t, os.MkdirAll(p.GuestsDir(), 0700))
	const snapshotID = "mac-fork-snapshot"
	source := *macOSGuestMetadata(2)
	source.Id, source.Name = "removed-source", "removed-source"
	source.DataDir = p.InstanceDir(source.Id)
	source.HypervisorType = hypervisor.TypeVZ
	source.Size, source.Vcpus = 8<<30, 4
	source.NetworkEnabled = true
	source.MAC = source.MacOS.MAC
	source.MacOS.GuestAgent = true
	source.StopGraceful = true
	now := time.Now()
	source.GuestAgentReadyAt = &now
	source.Phases.Record(phasetracking.PhaseRunning, now.Add(-time.Hour))
	source.Phases.Record(phasetracking.PhaseStopped, now)
	require.NoError(t, vz.NewStarter().ValidateConfig(m.plannedStoredVMConfig(SnapshotKindStopped, source)))
	require.NoError(t, m.saveSnapshotRecord(&snapshotRecord{Snapshot: Snapshot{Id: snapshotID, Kind: SnapshotKindStopped, SourceInstanceID: source.Id, SourceName: source.Name, SourceHypervisor: source.HypervisorType, CreatedAt: now}, StoredMetadata: source}))
	writeGuestFiles(t, p.SnapshotGuestDir(snapshotID), map[string]string{"overlay.raw": "snapshot-disk", macOSAuxStorageFile: "snapshot-aux"})

	first, err := m.ForkSnapshot(context.Background(), snapshotID, ForkSnapshotRequest{Name: "mac-snapshot-clone-1"})
	require.NoError(t, err)
	second, err := m.ForkSnapshot(context.Background(), snapshotID, ForkSnapshotRequest{Name: "mac-snapshot-clone-2"})
	require.NoError(t, err)
	for _, clone := range []*Instance{first, second} {
		require.Equal(t, StateStopped, clone.State)
		require.True(t, clone.IdentityRekeyPending)
		require.NotEqual(t, source.MacOS.MachineIdentifier, clone.MacOS.MachineIdentifier)
		require.NotEqual(t, source.MAC, clone.MAC)
		require.Equal(t, clone.MAC, clone.MacOS.MAC)
		require.Empty(t, clone.IP)
		require.Nil(t, clone.GuestAgentReadyAt)
		require.Nil(t, clone.HealthCheckRuntime)
		require.Empty(t, clone.Phases.Cumulative)
		require.Equal(t, phasetracking.PhaseStopped, clone.Phases.Current)
		require.Equal(t, clone.CreatedAt, clone.Phases.Since)
		data, err := os.ReadFile(filepath.Join(clone.DataDir, macOSAuxStorageFile))
		require.NoError(t, err)
		require.Equal(t, "snapshot-aux", string(data))
	}
	require.NotEqual(t, first.MAC, second.MAC)
	require.NotEqual(t, first.MacOS.MachineIdentifier, second.MacOS.MachineIdentifier)
	_, err = m.ForkSnapshot(context.Background(), snapshotID, ForkSnapshotRequest{Name: first.Name})
	require.ErrorIs(t, err, ErrAlreadyExists)
	persisted, err := m.loadSnapshotRecord(snapshotID)
	require.NoError(t, err)
	require.Equal(t, source.MacOS, persisted.StoredMetadata.MacOS)
	require.NoError(t, os.WriteFile(p.InstanceOverlay(first.Id), []byte("clone-write"), 0600))
	data, err := os.ReadFile(filepath.Join(p.SnapshotGuestDir(snapshotID), "overlay.raw"))
	require.NoError(t, err)
	require.Equal(t, "snapshot-disk", string(data))
}
