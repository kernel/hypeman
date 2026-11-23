package instances

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/kernel/hypeman/lib/devices"
	"github.com/kernel/hypeman/lib/forkvm"
	"github.com/kernel/hypeman/lib/hypervisor"
	"github.com/kernel/hypeman/lib/network"
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

type busyForkDevice struct{ devices.Manager }

func (busyForkDevice) ReleaseInstanceDevice(context.Context, string, string) error {
	return errors.New("device busy")
}

func TestRecoveryKeepsForkWhenPCIReleaseFails(t *testing.T) {
	m, _ := setupTestManager(t)
	const id = "pending-pci"
	createStoppedSnapshotSourceFixture(t, m, id, id, m.defaultHypervisor)
	meta, err := m.loadMetadata(id)
	require.NoError(t, err)
	meta.ForkPending = exitedProcessOwner(t)
	meta.Devices = []string{"dev-1"}
	require.NoError(t, m.saveMetadata(meta))
	m.deviceManager = busyForkDevice{m.deviceManager}

	m.RecoverInterruptedForks(t.Context())
	got, err := m.loadMetadata(id)
	require.NoError(t, err)
	require.Equal(t, meta.ForkPending, got.ForkPending)
}

func TestRecoveryRemovesOnlyVerifiedAliasesWithoutBackup(t *testing.T) {
	m, _ := setupTestManager(t)
	const source, fork = "deleted-source", "completed-fork"
	createStoppedSnapshotSourceFixture(t, m, fork, fork, m.defaultHypervisor)
	meta, err := m.loadMetadata(fork)
	require.NoError(t, err)
	meta.HypervisorType = hypervisor.TypeFirecracker
	require.NoError(t, m.saveMetadata(meta))
	sourceDir, forkDir := m.paths.InstanceDir(source), m.paths.InstanceDir(fork)
	data, err := json.Marshal(map[string]string{"snapshot_source_data_dir": sourceDir})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(forkDir, "firecracker-config.json"), data, 0644))
	require.NoError(t, os.Symlink(forkDir, sourceDir))
	// This unverified link is also an ID prefix of the fork. The exact lookup
	// must not fall through to prefix resolution.
	other := m.paths.InstanceDir("completed")
	require.NoError(t, os.Symlink(forkDir, other))
	require.NoError(t, RecoverParkedForkSources(m.paths))
	require.NoError(t, RecoverParkedForkSources(m.paths))
	_, err = os.Lstat(sourceDir)
	require.ErrorIs(t, err, os.ErrNotExist)
	target, err := os.Readlink(other)
	require.NoError(t, err)
	require.Equal(t, forkDir, target)
	for _, id := range []string{source, "completed"} {
		_, err := m.GetInstance(t.Context(), id)
		require.ErrorIs(t, err, ErrNotFound)
	}
	got, err := m.GetInstance(t.Context(), fork)
	require.NoError(t, err)
	require.Equal(t, fork, got.Id)
}

func TestForkCopyOmitsSourceMetadata(t *testing.T) {
	m, _ := setupTestManager(t)
	const source, fork = "source", "fork"
	createStoppedSnapshotSourceFixture(t, m, source, source, m.defaultHypervisor)
	require.NoError(t, m.cloneGuestDirectoryForFork(t.Context(), m.paths.InstanceDir(source), m.paths.InstanceDir(fork), false))
	require.FileExists(t, m.paths.InstanceOverlay(fork))
	require.NoFileExists(t, m.paths.InstanceMetadata(fork))
}

func TestTTLReaperSkipsForeignMetadata(t *testing.T) {
	m, _ := setupTestManager(t)
	const source, fork = "source", "partial-copy"
	past, future := time.Now().Add(-time.Hour), time.Now().Add(time.Hour)
	meta := &metadata{StoredMetadata: StoredMetadata{Id: source, ExpiresAt: &past}}
	require.NoError(t, m.ensureDirectories(source))
	require.NoError(t, m.saveMetadata(meta))
	// Older fork copies can still contain the source's previous TTL.
	require.NoError(t, forkvm.CopyGuestDirectory(m.paths.InstanceDir(source), m.paths.InstanceDir(fork)))
	meta.ExpiresAt = &future
	require.NoError(t, m.saveMetadata(meta))

	var deletes []string
	m.deleteInstanceFn = func(_ context.Context, id string) error {
		deletes = append(deletes, id)
		return nil
	}
	m.reapExpiredInstances(t.Context())
	require.Empty(t, deletes)
}

type forkTAPRelease struct {
	network.Manager
	ids []string
	err error
}

func (r *forkTAPRelease) ReleaseByInstanceID(_ context.Context, id string) error {
	r.ids = append(r.ids, id)
	return r.err
}

func createPendingForkWithoutAllocation(t *testing.T, m *manager, fork string) {
	t.Helper()
	createStoppedSnapshotSourceFixture(t, m, fork, fork, m.defaultHypervisor)
	meta, err := m.loadMetadata(fork)
	require.NoError(t, err)
	meta.NetworkEnabled = true
	meta.ForkPending = exitedProcessOwner(t)
	require.NoError(t, m.saveMetadata(meta))
	alloc, err := m.networkManager.GetAllocation(t.Context(), fork)
	require.NoError(t, err)
	require.Nil(t, alloc)
}

// The TAP reconciler deletes a TAP that no live instance claims, so a fork is
// not kept for one that could not be released.
func TestRecoveryDeletesForkWhenTAPReleaseFails(t *testing.T) {
	m, _ := setupTestManager(t)
	const fork = "fork-no-allocation"
	createPendingForkWithoutAllocation(t, m, fork)
	release := &forkTAPRelease{Manager: m.networkManager, err: errors.New("TAP busy")}
	m.networkManager = release

	m.RecoverInterruptedForks(t.Context())
	require.Equal(t, []string{fork}, release.ids)
	require.NoDirExists(t, m.paths.InstanceDir(fork))
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
	for _, layout := range []string{"link", "source-absent", "dangling-link", "unrelated-link", "live-owner", "existing-directory", "two-backups"} {
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
			if layout == "dangling-link" {
				// The fork was deleted by a binary that does not recover the link.
				require.NoError(t, os.RemoveAll(forkDir))
			}
			if layout == "two-backups" {
				require.NoError(t, os.Mkdir(sourceDir+".fork-bak.124", 0755))
			}
			err = RecoverParkedForkSources(m.paths)
			if layout != "link" && layout != "source-absent" && layout != "dangling-link" {
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
