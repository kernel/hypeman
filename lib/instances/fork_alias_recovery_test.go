package instances

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kernel/hypeman/lib/forkvm"
	"github.com/kernel/hypeman/lib/hypervisor"
	"github.com/stretchr/testify/require"
)

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

func TestRecoveryRestoresParkedFirecrackerSource(t *testing.T) {
	for _, layout := range []string{"link", "source-absent", "dangling-link", "unrelated-link", "existing-directory", "two-backups"} {
		t.Run(layout, func(t *testing.T) {
			m, _ := setupTestManager(t)
			const source, fork = "original", "interrupted"
			for _, id := range []string{source, fork} {
				createStoppedSnapshotSourceFixture(t, m, id, id, m.defaultHypervisor)
			}
			meta, err := m.loadMetadata(fork)
			require.NoError(t, err)
			meta.HypervisorType = hypervisor.TypeFirecracker
			meta.ForkPending = true
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

// An older fork copy cut short before the fork saved its own metadata holds
// the source's. It cannot have a VMM, so it must not fail the listing that
// vGPU claims and reconciles depend on, whether or not its source still exists.
func TestReconcileListingSkipsDirectoryHoldingAnotherInstancesMetadata(t *testing.T) {
	m, _ := setupTestManager(t)
	const source, deleted = "source", "deleted-source"
	for _, id := range []string{source, deleted} {
		createStoppedSnapshotSourceFixture(t, m, id, id, m.defaultHypervisor)
		require.NoError(t, forkvm.CopyGuestDirectory(m.paths.InstanceDir(id), m.paths.InstanceDir(id+"-copy")))
	}
	require.NoError(t, os.RemoveAll(m.paths.InstanceDir(deleted)))

	listed, err := m.listMetadataForReconcile()
	require.NoError(t, err)
	require.Len(t, listed, 1)
	require.Equal(t, source, listed[0].Id)
}

// One source that cannot be recovered must not keep the others parked.
func TestParkedSourceRecoveryGoesOnPastASourceItRefuses(t *testing.T) {
	m, _ := setupTestManager(t)
	const refused, parked, fork = "refused", "parked", "interrupted"
	for _, id := range []string{refused, parked, fork} {
		createStoppedSnapshotSourceFixture(t, m, id, id, m.defaultHypervisor)
	}
	meta, err := m.loadMetadata(fork)
	require.NoError(t, err)
	meta.HypervisorType = hypervisor.TypeFirecracker
	meta.ForkPending = true
	require.NoError(t, m.saveMetadata(meta))
	forkDir := m.paths.InstanceDir(fork)
	data, err := json.Marshal(map[string]string{"snapshot_source_data_dir": m.paths.InstanceDir(parked)})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(forkDir, "firecracker-config.json"), data, 0644))

	for _, id := range []string{refused, parked} {
		dir := m.paths.InstanceDir(id)
		require.NoError(t, os.Rename(dir, dir+".fork-bak.123"))
		require.NoError(t, os.Symlink(forkDir, dir))
	}
	// The fork records only one of them as its source.
	refusedDir := m.paths.InstanceDir(refused)

	err = RecoverParkedForkSources(m.paths)
	require.ErrorContains(t, err, "restore "+refused)
	require.NotContains(t, err.Error(), "restore "+parked)

	got, err := m.GetInstance(t.Context(), parked)
	require.NoError(t, err)
	require.Equal(t, parked, got.Id)
	link, err := os.Readlink(refusedDir)
	require.NoError(t, err)
	require.Equal(t, forkDir, link)
	require.DirExists(t, refusedDir+".fork-bak.123")
	_, err = m.GetInstance(t.Context(), refused)
	require.ErrorIs(t, err, ErrNotFound)
}
