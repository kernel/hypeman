package instances

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kernel/hypeman/lib/hypervisor"
	"github.com/kernel/hypeman/lib/vmm"
	"github.com/stretchr/testify/require"
)

// TestRestoreInstanceMissingImageReturnsNotFoundFast verifies that restoring a
// standby instance whose image was deleted fails fast with images.ErrNotFound
// (so the handler maps it to 404) instead of hanging in the hypervisor shim and
// surfacing a generic 500.
func TestRestoreInstanceMissingImageReturnsNotFoundFast(t *testing.T) {
	mgr, tmpDir := setupTestManager(t)

	id := "restore-missing-image"
	stored := &StoredMetadata{
		Id:                id,
		Name:              "restore-missing-image",
		Image:             "docker.io/library/missing:latest",
		Size:              1024 * 1024 * 1024,
		Vcpus:             1,
		CreatedAt:         time.Now(),
		HypervisorType:    hypervisor.TypeCloudHypervisor,
		HypervisorVersion: string(vmm.V51_1),
		// A socket path that does not exist forces deriveState down the
		// no-socket branch, where a present snapshot dir yields Standby.
		SocketPath: filepath.Join(tmpDir, id, "nonexistent.sock"),
		DataDir:    mgr.paths.InstanceDir(id),
	}

	require.NoError(t, mgr.ensureDirectories(id))
	// Materialize a non-empty snapshot-latest dir so the instance derives to
	// Standby with HasSnapshot=true, reaching the restore image pre-check.
	snapshotDir := filepath.Join(stored.DataDir, "snapshots", "snapshot-latest")
	require.NoError(t, os.MkdirAll(snapshotDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(snapshotDir, "config.json"), []byte("{}"), 0o644))

	require.NoError(t, mgr.saveMetadata(&metadata{StoredMetadata: *stored}))

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// The pre-check short-circuits before the ~60s shim retry loop.
	assertFailsFastNotFound(t, "restore should fail fast, not hang in the shim", func() error {
		_, err := mgr.restoreInstance(ctx, id)
		return err
	})
}
