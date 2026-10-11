package instances

import (
	"context"
	"os"
	"testing"

	"github.com/kernel/hypeman/lib/hypervisor"
	"github.com/kernel/hypeman/lib/paths"
	"github.com/stretchr/testify/require"
)

func TestMacOSSnapshotTargetUsesMacOSBootConfig(t *testing.T) {
	p := paths.New(t.TempDir())
	m := &manager{paths: p}
	meta := *macOSGuestMetadata(2)
	meta.Id, meta.DataDir = "mac-snapshot", p.InstanceDir("mac-snapshot")
	meta.Size, meta.Vcpus = 4<<30, 3
	for _, kind := range []SnapshotKind{SnapshotKindStopped, SnapshotKindStandby} {
		config := m.plannedStoredVMConfig(kind, meta)
		require.Equal(t, hypervisor.BootModeMacOS, config.BootMode)
		require.Equal(t, meta.Size, config.MemoryBytes)
		require.Equal(t, meta.Vcpus, config.VCPUs)
		require.Len(t, config.Disks, 1)
		require.Empty(t, config.KernelPath)
		require.False(t, config.GuestMemory.EnableBalloon)
		require.NoError(t, hypervisor.ValidateBootConfig(config))
		require.Error(t, hypervisor.ValidateDirectRawConfig("linux-backend", config))
	}
}

func TestMacOSSnapshotForkRejectsLinuxBackendBeforeCopy(t *testing.T) {
	p := paths.New(t.TempDir())
	m := &manager{paths: p, macOSOnly: true, vmStarters: map[hypervisor.Type]hypervisor.VMStarter{hypervisor.TypeFirecracker: macOSSnapshotLinuxStarter{}}}
	require.NoError(t, os.MkdirAll(p.GuestsDir(), 0700))
	source := *macOSGuestMetadata(2)
	source.Id = "mac-source"
	source.HypervisorType = hypervisor.TypeVZ
	source.StopGraceful = true
	source.MacOS.GuestAgent = true
	require.NoError(t, m.saveSnapshotRecord(&snapshotRecord{Snapshot: Snapshot{Id: "mac-snapshot", Kind: SnapshotKindStopped, SourceInstanceID: source.Id, SourceHypervisor: hypervisor.TypeVZ}, StoredMetadata: source}))
	_, err := m.ForkSnapshot(context.Background(), "mac-snapshot", ForkSnapshotRequest{Name: "linux-target", TargetHypervisor: hypervisor.TypeFirecracker})
	require.ErrorIs(t, err, ErrInvalidRequest)
	entries, err := os.ReadDir(p.GuestsDir())
	require.NoError(t, err)
	require.Empty(t, entries, "backend admission rejects the fork before copying files")
}

type macOSSnapshotLinuxStarter struct {
	hypervisor.VMStarter
}

func (macOSSnapshotLinuxStarter) ValidateConfig(config hypervisor.VMConfig) error {
	return hypervisor.ValidateDirectRawConfig("linux-test", config)
}
