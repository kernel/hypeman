package instances

import (
	"encoding/json"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/kernel/hypeman/lib/devices"
	"github.com/kernel/hypeman/lib/hypervisor"
	"github.com/stretchr/testify/require"
)

func TestRecoveryLeavesInheritedPCIDeviceWithOwner(t *testing.T) {
	m, _ := setupTestManager(t)
	const owner, fork, dev = "source", "interrupted", "dev-1"
	createStoppedSnapshotSourceFixture(t, m, fork, fork, m.defaultHypervisor)
	meta, err := m.loadMetadata(fork)
	require.NoError(t, err)
	meta.ForkPending = exitedProcessOwner(t)
	meta.Devices = []string{dev}
	require.NoError(t, m.saveMetadata(meta))
	source := owner
	data, err := json.Marshal(devices.Device{Id: dev, AttachedTo: &source, BoundToVFIO: true, PCIAddress: "test-device"})
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(m.paths.DeviceDir(dev), 0755))
	require.NoError(t, os.WriteFile(m.paths.DeviceMetadata(dev), data, 0644))

	m.RecoverInterruptedForks(t.Context())
	require.NoDirExists(t, m.paths.InstanceDir(fork))
	after, err := os.ReadFile(m.paths.DeviceMetadata(dev))
	require.NoError(t, err)
	require.JSONEq(t, string(data), string(after))
}

func TestRestoreSnapshotCannotReplacePendingFork(t *testing.T) {
	m, _ := setupTestManager(t)
	const id = "pending"
	createStoppedSnapshotSourceFixture(t, m, id, id, m.defaultHypervisor)
	meta, err := m.loadMetadata(id)
	require.NoError(t, err)
	meta.ForkPending = currentForkOwner()
	require.NoError(t, m.saveMetadata(meta))
	snap, err := m.CreateSnapshot(t.Context(), id, CreateSnapshotRequest{Kind: SnapshotKindStopped})
	require.NoError(t, err)
	keep := filepath.Join(m.paths.InstanceDir(id), "written-after-snapshot")
	require.NoError(t, os.WriteFile(keep, nil, 0644))

	_, err = m.RestoreSnapshot(t.Context(), id, snap.Id, RestoreSnapshotRequest{})
	require.ErrorIs(t, err, ErrInvalidState)
	require.FileExists(t, keep)
	got, err := m.loadMetadata(id)
	require.NoError(t, err)
	require.Equal(t, meta.ForkPending, got.ForkPending)
}

func TestSnapshotPayloadNeverCopiesPendingMark(t *testing.T) {
	m, _ := setupTestManager(t)
	const id = "pending"
	createStoppedSnapshotSourceFixture(t, m, id, id, m.defaultHypervisor)
	meta, err := m.loadMetadata(id)
	require.NoError(t, err)
	meta.ForkPending = currentForkOwner()
	require.NoError(t, m.saveMetadata(meta))
	// Stop the copy after the walk has visited metadata.json.
	require.NoError(t, syscall.Mkfifo(filepath.Join(m.paths.InstanceDir(id), "zz-fifo"), 0600))
	dst := t.TempDir()
	require.ErrorContains(t, m.copySnapshotPayload(id, dst), "unsupported file type")
	require.NoFileExists(t, filepath.Join(dst, "metadata.json"))
}

func TestRecoveryRestoresParkedFirecrackerSource(t *testing.T) {
	for _, layout := range []string{"link", "source-absent", "unrelated-link", "live-owner", "existing-directory", "two-backups"} {
		t.Run(layout, func(t *testing.T) {
			m, _ := setupTestManager(t)
			const source, fork = "original", "interrupted"
			for _, id := range []string{source, fork} {
				createStoppedSnapshotSourceFixture(t, m, id, id, m.defaultHypervisor)
			}
			meta, err := m.loadMetadata(fork)
			require.NoError(t, err)
			meta.HypervisorType = hypervisor.TypeFirecracker
			meta.ForkPending = exitedProcessOwner(t)
			if layout == "live-owner" {
				meta.ForkPending = currentForkOwner()
			}
			require.NoError(t, m.saveMetadata(meta))
			sourceDir, forkDir := m.paths.InstanceDir(source), m.paths.InstanceDir(fork)
			recorded := sourceDir
			if layout == "unrelated-link" {
				recorded = m.paths.InstanceDir("other")
			}
			data, err := json.Marshal(map[string]string{"snapshot_source_data_dir": recorded})
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(filepath.Join(forkDir, "firecracker-config.json"), data, 0644))

			backup := sourceDir + ".fork-bak.123"
			require.NoError(t, os.Rename(sourceDir, backup))
			switch layout {
			case "source-absent":
			case "existing-directory":
				require.NoError(t, os.Mkdir(sourceDir, 0755))
			default:
				require.NoError(t, os.Symlink(forkDir, sourceDir))
			}
			if layout == "two-backups" {
				require.NoError(t, os.Mkdir(sourceDir+".fork-bak.124", 0755))
			}
			err = RecoverParkedForkSources(m.paths)
			if layout != "link" && layout != "source-absent" {
				require.Error(t, err)
				require.DirExists(t, backup)
				require.DirExists(t, forkDir)
				if layout == "existing-directory" {
					require.ErrorContains(t, err, "is not an alias link")
					require.NoFileExists(t, m.paths.InstanceMetadata(source))
				} else {
					link, err := os.Readlink(sourceDir)
					require.NoError(t, err)
					require.Equal(t, forkDir, link)
				}
				return
			}
			require.NoError(t, err)
			require.NoError(t, RecoverParkedForkSources(m.paths), "repeat startup")
			m.RecoverInterruptedForks(t.Context())
			got, err := m.loadMetadata(source)
			require.NoError(t, err)
			require.Equal(t, source, got.Id)
			require.NoDirExists(t, backup)
			require.NoDirExists(t, forkDir)
		})
	}
}
