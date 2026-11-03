package instances

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/kernel/hypeman/lib/hypervisor"
	"github.com/kernel/hypeman/lib/paths"
	"github.com/stretchr/testify/require"
)

// useMacOSAgentDisk makes the host's agent disk a file holding content.
func useMacOSAgentDisk(t *testing.T, content string) {
	t.Helper()
	disk := filepath.Join(t.TempDir(), "agent.img")
	require.NoError(t, os.WriteFile(disk, []byte(content), 0644))
	prev := macOSAgentDiskSource
	macOSAgentDiskSource = func(context.Context, *paths.Paths) (string, error) { return disk, nil }
	t.Cleanup(func() { macOSAgentDiskSource = prev })
}

func TestMacOSAgentDiskInstallAndRefresh(t *testing.T) {
	m := &manager{paths: paths.New(t.TempDir())}
	stored := &StoredMetadata{Id: "mac", MacOS: macOSGuestMetadata(2).MacOS}
	require.NoError(t, os.MkdirAll(m.paths.InstanceDir(stored.Id), 0700))

	useMacOSAgentDisk(t, "agent v1")
	require.NoError(t, m.installMacOSAgentDisk(context.Background(), stored))
	require.NoFileExists(t, m.paths.InstanceMacOSAgentDisk(stored.Id), "a guest without the flag gets no agent disk")

	stored.MacOSAgentDisk = true
	require.NoError(t, m.installMacOSAgentDisk(context.Background(), stored))
	data, err := os.ReadFile(m.paths.InstanceMacOSAgentDisk(stored.Id))
	require.NoError(t, err)
	require.Equal(t, "agent v1", string(data))
	info, err := os.Stat(m.paths.InstanceMacOSAgentDisk(stored.Id))
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0400), info.Mode().Perm())

	useMacOSAgentDisk(t, "agent v2")
	require.NoError(t, m.installMacOSAgentDisk(context.Background(), stored), "a cold start replaces the guest's copy")
	data, err = os.ReadFile(m.paths.InstanceMacOSAgentDisk(stored.Id))
	require.NoError(t, err)
	require.Equal(t, "agent v2", string(data))
}

func TestMacOSDisksAttachAgentDiskSecond(t *testing.T) {
	p := paths.New(t.TempDir())
	stored := &StoredMetadata{Id: "mac", DataDir: p.InstanceDir("mac"), MacOSDiskLayers: 1}
	require.Equal(t, []hypervisor.DiskConfig{{Path: p.InstanceMacOSBootDisk("mac"), Parents: []string{macOSDiskLayer(stored.DataDir, 0)}}}, macOSDisks(p, stored))
	stored.MacOSAgentDisk = true
	disks := macOSDisks(p, stored)
	require.Len(t, disks, 2)
	require.Equal(t, p.InstanceMacOSBootDisk("mac"), disks[0].Path, "the boot disk stays first")
	require.Equal(t, hypervisor.DiskConfig{Path: p.InstanceMacOSAgentDisk("mac"), Readonly: true}, disks[1])
}

func TestMacOSMemorySnapshotRestoresAgentDiskItWasSavedWith(t *testing.T) {
	m, meta := macOSMemorySnapshotFixture(t)
	meta.MacOSAgentDisk = true
	require.NoError(t, m.saveMetadata(meta))
	agentDisk := m.paths.InstanceMacOSAgentDisk(meta.Id)
	require.NoError(t, os.WriteFile(agentDisk, []byte("agent-at-checkpoint"), 0400))
	writeMacOSMemorySnapshotPayload(t, m, meta.DataDir, &meta.StoredMetadata)
	ctx := context.Background()
	snapshot, err := m.CreateSnapshot(ctx, meta.Id, CreateSnapshotRequest{Kind: SnapshotKindStandby})
	require.NoError(t, err)

	require.NoError(t, os.Remove(agentDisk))
	require.NoError(t, os.WriteFile(agentDisk, []byte("agent-after"), 0400))
	_, err = m.RestoreSnapshot(ctx, meta.Id, snapshot.Id, RestoreSnapshotRequest{TargetState: StateStandby})
	require.NoError(t, err)
	data, err := os.ReadFile(agentDisk)
	require.NoError(t, err)
	require.Equal(t, "agent-at-checkpoint", string(data), "saved state restores with the agent disk it was saved with")
}

func TestMacOSMemorySnapshotRequiresItsAgentDisk(t *testing.T) {
	m, meta := macOSMemorySnapshotFixture(t)
	meta.MacOSAgentDisk = true
	require.NoError(t, m.saveMetadata(meta))
	writeMacOSMemorySnapshotPayload(t, m, meta.DataDir, &meta.StoredMetadata)
	_, err := m.CreateSnapshot(context.Background(), meta.Id, CreateSnapshotRequest{Kind: SnapshotKindStandby})
	require.ErrorIs(t, err, ErrInvalidRequest, "a payload without its agent disk is refused")
}
