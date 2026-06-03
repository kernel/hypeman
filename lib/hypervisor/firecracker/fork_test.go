package firecracker

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/kernel/hypeman/lib/hypervisor"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPrepareFork_NoSnapshotPathIsSupported(t *testing.T) {
	starter := NewStarter()
	result, err := starter.PrepareFork(context.Background(), hypervisor.ForkPrepareRequest{})
	require.NoError(t, err)
	assert.False(t, result.VsockCIDUpdated)
}

func TestPrepareFork_SnapshotRewritePersistsRestoreMetadata(t *testing.T) {
	starter := NewStarter()
	tmp := t.TempDir()
	targetDir := filepath.Join(tmp, "target")
	require.NoError(t, os.MkdirAll(targetDir, 0755))
	require.NoError(t, saveRestoreMetadata(targetDir, []networkInterface{{IfaceID: "eth0", HostDevName: "tap-old"}}))

	_, err := starter.PrepareFork(context.Background(), hypervisor.ForkPrepareRequest{
		SnapshotConfigPath: filepath.Join(targetDir, "snapshots", "snapshot-latest", "config.json"),
		SourceDataDir:      filepath.Join(tmp, "source"),
		TargetDataDir:      targetDir,
		Network: &hypervisor.ForkNetworkConfig{
			TAPDevice: "tap-new",
		},
	})
	require.NoError(t, err)

	meta, err := loadRestoreMetadata(targetDir)
	require.NoError(t, err)
	require.Len(t, meta.NetworkOverrides, 1)
	assert.Equal(t, "tap-new", meta.NetworkOverrides[0].HostDevName)
	assert.Equal(t, filepath.Join(tmp, "source"), meta.SnapshotSourceDataDir)
	assert.True(t, meta.RetainSnapshotSourceDataDirAlias)
}

func TestPrepareFork_DoesNotRetainExistingSourceAlias(t *testing.T) {
	starter := NewStarter()
	tmp := t.TempDir()
	sourceDir := filepath.Join(tmp, "source")
	targetDir := filepath.Join(tmp, "target")
	require.NoError(t, os.MkdirAll(sourceDir, 0755))
	require.NoError(t, os.MkdirAll(targetDir, 0755))
	require.NoError(t, saveRestoreMetadata(targetDir, nil))

	_, err := starter.PrepareFork(context.Background(), hypervisor.ForkPrepareRequest{
		SnapshotConfigPath: filepath.Join(targetDir, "snapshots", "snapshot-latest", "config.json"),
		SourceDataDir:      sourceDir,
		TargetDataDir:      targetDir,
	})
	require.NoError(t, err)

	meta, err := loadRestoreMetadata(targetDir)
	require.NoError(t, err)
	assert.Equal(t, sourceDir, meta.SnapshotSourceDataDir)
	assert.False(t, meta.RetainSnapshotSourceDataDirAlias)
}

func TestPrepareFork_RetainsExistingSourceAliasWhenRequested(t *testing.T) {
	starter := NewStarter()
	tmp := t.TempDir()
	sourceDir := filepath.Join(tmp, "source")
	targetDir := filepath.Join(tmp, "target")
	require.NoError(t, os.MkdirAll(sourceDir, 0755))
	require.NoError(t, os.MkdirAll(targetDir, 0755))
	require.NoError(t, saveRestoreMetadata(targetDir, nil))

	_, err := starter.PrepareFork(context.Background(), hypervisor.ForkPrepareRequest{
		SnapshotConfigPath:               filepath.Join(targetDir, "snapshots", "snapshot-latest", "config.json"),
		SourceDataDir:                    sourceDir,
		TargetDataDir:                    targetDir,
		RetainSnapshotSourceDataDirAlias: true,
	})
	require.NoError(t, err)

	meta, err := loadRestoreMetadata(targetDir)
	require.NoError(t, err)
	assert.Equal(t, sourceDir, meta.SnapshotSourceDataDir)
	assert.True(t, meta.RetainSnapshotSourceDataDirAlias)
}

func TestPrepareFork_ReturnsSourceStatErrors(t *testing.T) {
	starter := NewStarter()
	tmp := t.TempDir()
	targetDir := filepath.Join(tmp, "target")
	require.NoError(t, os.MkdirAll(targetDir, 0755))
	require.NoError(t, saveRestoreMetadata(targetDir, nil))

	_, err := starter.PrepareFork(context.Background(), hypervisor.ForkPrepareRequest{
		SnapshotConfigPath: filepath.Join(targetDir, "snapshots", "snapshot-latest", "config.json"),
		SourceDataDir:      filepath.Join(tmp, "source") + "\x00",
		TargetDataDir:      targetDir,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "stat snapshot source data dir")
}

func TestPrepareFork_PreservesRetainedUpstreamAlias(t *testing.T) {
	starter := NewStarter()
	tmp := t.TempDir()
	upstreamDir := filepath.Join(tmp, "upstream")
	sourceDir := filepath.Join(tmp, "source")
	targetDir := filepath.Join(tmp, "target")
	require.NoError(t, os.MkdirAll(sourceDir, 0755))
	require.NoError(t, os.MkdirAll(targetDir, 0755))
	require.NoError(t, saveRestoreMetadataState(targetDir, &restoreMetadata{
		SnapshotSourceDataDir:            upstreamDir,
		RetainSnapshotSourceDataDirAlias: true,
	}))

	_, err := starter.PrepareFork(context.Background(), hypervisor.ForkPrepareRequest{
		SnapshotConfigPath: filepath.Join(targetDir, "snapshots", "snapshot-latest", "config.json"),
		SourceDataDir:      sourceDir,
		TargetDataDir:      targetDir,
	})
	require.NoError(t, err)

	meta, err := loadRestoreMetadata(targetDir)
	require.NoError(t, err)
	assert.Equal(t, upstreamDir, meta.SnapshotSourceDataDir)
	assert.True(t, meta.RetainSnapshotSourceDataDirAlias)
}

func TestPrepareFork_NetworkRewritePreservesRetainedAlias(t *testing.T) {
	starter := NewStarter()
	tmp := t.TempDir()
	upstreamDir := filepath.Join(tmp, "upstream")
	targetDir := filepath.Join(tmp, "target")
	require.NoError(t, os.MkdirAll(targetDir, 0755))
	require.NoError(t, saveRestoreMetadataState(targetDir, &restoreMetadata{
		SnapshotSourceDataDir:            upstreamDir,
		RetainSnapshotSourceDataDirAlias: true,
		NetworkOverrides: []networkOverride{{
			IfaceID:     "eth0",
			HostDevName: "tap-old",
		}},
	}))

	_, err := starter.PrepareFork(context.Background(), hypervisor.ForkPrepareRequest{
		SnapshotConfigPath: filepath.Join(targetDir, "snapshots", "snapshot-latest", "config.json"),
		Network: &hypervisor.ForkNetworkConfig{
			TAPDevice: "tap-new",
		},
	})
	require.NoError(t, err)

	meta, err := loadRestoreMetadata(targetDir)
	require.NoError(t, err)
	require.Len(t, meta.NetworkOverrides, 1)
	assert.Equal(t, "tap-new", meta.NetworkOverrides[0].HostDevName)
	assert.Equal(t, upstreamDir, meta.SnapshotSourceDataDir)
	assert.True(t, meta.RetainSnapshotSourceDataDirAlias)
}
