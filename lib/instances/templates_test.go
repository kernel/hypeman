package instances

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kernel/hypeman/cmd/api/config"
	"github.com/kernel/hypeman/lib/devices"
	"github.com/kernel/hypeman/lib/hypervisor"
	"github.com/kernel/hypeman/lib/images"
	"github.com/kernel/hypeman/lib/network"
	"github.com/kernel/hypeman/lib/paths"
	"github.com/kernel/hypeman/lib/system"
	"github.com/kernel/hypeman/lib/vmm"
	"github.com/kernel/hypeman/lib/volumes"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newStorageOnlyManager wires up just enough of the manager to exercise
// metadata + filesystem behavior without needing real VMs or images.
func newStorageOnlyManager(t *testing.T) (*manager, string) {
	t.Helper()
	tmpDir := t.TempDir()

	cfg := &config.Config{
		DataDir: tmpDir,
		Network: newParallelTestNetworkConfig(t),
		Oversubscription: config.OversubscriptionConfig{
			CPU: 1.0, Memory: 1.0, Disk: 1.0, Network: 1.0,
		},
	}

	p := paths.New(tmpDir)
	imageManager, _ := images.NewManager(p, 1, nil)
	systemManager := system.NewManager(p)
	networkManager := network.NewManager(p, cfg, nil)
	deviceManager := devices.NewManager(p)
	volumeManager := volumes.NewManager(p, 0, nil)
	limits := ResourceLimits{
		MaxOverlaySize: 100 * 1024 * 1024 * 1024,
	}
	mgr := NewManager(p, imageManager, systemManager, networkManager, deviceManager, volumeManager, limits, "", SnapshotPolicy{}, nil, nil).(*manager)
	return mgr, tmpDir
}

// writeTemplateMetadata writes a metadata.json marked as a template with a
// fake snapshot directory so deriveState returns StateTemplate, plus the
// requested number of fork metadata files pointing at it.
func writeTemplateMetadata(t *testing.T, mgr *manager, id string, forkCount int) {
	t.Helper()
	require.NoError(t, mgr.ensureDirectories(id))
	snapshotDir := filepath.Join(mgr.paths.InstanceDir(id), "snapshots", "snapshot-latest")
	require.NoError(t, os.MkdirAll(snapshotDir, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(snapshotDir, "config.json"), []byte("{}"), 0644))

	meta := &metadata{
		StoredMetadata: StoredMetadata{
			Id:                id,
			Name:              id,
			Image:             "test:latest",
			Vcpus:             1,
			Size:              1024 * 1024 * 1024,
			CreatedAt:         time.Now(),
			HypervisorType:    hypervisor.TypeCloudHypervisor,
			SocketPath:        filepath.Join(mgr.paths.InstanceDir(id), "ch.sock"),
			DataDir:           mgr.paths.InstanceDir(id),
			HypervisorVersion: string(vmm.V49_0),
			IsTemplate:        true,
		},
	}
	require.NoError(t, mgr.saveMetadata(meta))

	for i := 0; i < forkCount; i++ {
		writeForkMetadata(t, mgr, fmt.Sprintf("%s-fork-%d", id, i), id)
	}
}

// writeForkMetadata writes a Stopped fork metadata file pointing at templateID.
func writeForkMetadata(t *testing.T, mgr *manager, id, templateID string) {
	t.Helper()
	require.NoError(t, mgr.ensureDirectories(id))
	meta := &metadata{
		StoredMetadata: StoredMetadata{
			Id:                id,
			Name:              id,
			Image:             "test:latest",
			Vcpus:             1,
			Size:              1024 * 1024 * 1024,
			CreatedAt:         time.Now(),
			HypervisorType:    hypervisor.TypeCloudHypervisor,
			SocketPath:        filepath.Join(mgr.paths.InstanceDir(id), "ch.sock"),
			DataDir:           mgr.paths.InstanceDir(id),
			HypervisorVersion: string(vmm.V49_0),
			ForkOfTemplate:    templateID,
		},
	}
	require.NoError(t, mgr.saveMetadata(meta))
}

func TestDeriveStateTemplate(t *testing.T) {
	t.Parallel()
	mgr, _ := newStorageOnlyManager(t)
	ctx := context.Background()

	writeTemplateMetadata(t, mgr, "tmpl-1", 2)

	inst, err := mgr.GetInstance(ctx, "tmpl-1")
	require.NoError(t, err)
	assert.Equal(t, StateTemplate, inst.State)
	assert.True(t, inst.IsTemplate)
}

func TestCountTemplateForks(t *testing.T) {
	t.Parallel()
	mgr, _ := newStorageOnlyManager(t)

	writeTemplateMetadata(t, mgr, "tmpl-a", 3)
	writeTemplateMetadata(t, mgr, "tmpl-b", 1)

	got, err := mgr.countTemplateForks("tmpl-a")
	require.NoError(t, err)
	assert.Equal(t, 3, got)

	got, err = mgr.countTemplateForks("tmpl-b")
	require.NoError(t, err)
	assert.Equal(t, 1, got)

	got, err = mgr.countTemplateForks("tmpl-missing")
	require.NoError(t, err)
	assert.Equal(t, 0, got)
}

func TestRestoreInstanceTemplateWithForksRefused(t *testing.T) {
	t.Parallel()
	mgr, _ := newStorageOnlyManager(t)
	ctx := context.Background()

	writeTemplateMetadata(t, mgr, "tmpl-busy", 1)

	_, err := mgr.RestoreInstance(ctx, "tmpl-busy")
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrInvalidState), "expected ErrInvalidState, got %v", err)
}

func TestRestoreInstanceTemplateDemotes(t *testing.T) {
	t.Parallel()
	mgr, _ := newStorageOnlyManager(t)
	ctx := context.Background()

	writeTemplateMetadata(t, mgr, "tmpl-idle", 0)

	// Restore will fail downstream because there's no real snapshot to restore,
	// but the demotion step should clear IsTemplate and HotPagesPath before that.
	require.NoError(t, mgr.demoteTemplate(ctx, "tmpl-idle"))

	loaded, err := mgr.loadMetadata("tmpl-idle")
	require.NoError(t, err)
	assert.False(t, loaded.IsTemplate)
	assert.Empty(t, loaded.HotPagesPath)

	inst, err := mgr.GetInstance(ctx, "tmpl-idle")
	require.NoError(t, err)
	assert.Equal(t, StateStandby, inst.State)
}

func TestDeleteInstanceTemplateWithForksRefused(t *testing.T) {
	t.Parallel()
	mgr, _ := newStorageOnlyManager(t)
	ctx := context.Background()

	writeTemplateMetadata(t, mgr, "tmpl-with-forks", 3)

	err := mgr.DeleteInstance(ctx, "tmpl-with-forks")
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrInvalidState), "expected ErrInvalidState, got %v", err)

	loaded, err := mgr.loadMetadata("tmpl-with-forks")
	require.NoError(t, err)
	assert.True(t, loaded.IsTemplate)
}

func TestDeleteForkLeavesTemplateIntact(t *testing.T) {
	t.Parallel()
	mgr, _ := newStorageOnlyManager(t)
	ctx := context.Background()

	writeTemplateMetadata(t, mgr, "tmpl-parent", 2)

	require.NoError(t, mgr.DeleteInstance(ctx, "tmpl-parent-fork-0"))

	parent, err := mgr.loadMetadata("tmpl-parent")
	require.NoError(t, err)
	assert.True(t, parent.IsTemplate)

	got, err := mgr.countTemplateForks("tmpl-parent")
	require.NoError(t, err)
	assert.Equal(t, 1, got)
}
