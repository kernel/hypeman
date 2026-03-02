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
}
