//go:build darwin

package instances

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/kernel/hypeman/lib/paths"
	"github.com/stretchr/testify/require"
)

func TestMacOSBuildStorageRequiresClosedDistinctFiles(t *testing.T) {
	root := t.TempDir()
	storage := MacOSStorage{
		Layers:  []string{filepath.Join(root, "disk.0.asif")},
		Overlay: filepath.Join(root, paths.MacOSBootDiskFile),
		Aux:     filepath.Join(root, "mac-aux.img"),
	}
	for _, path := range []string{storage.Layers[0], storage.Overlay, storage.Aux} {
		require.NoError(t, os.WriteFile(path, []byte("synthetic"), 0600))
	}
	require.NoError(t, checkClosedMacOSStorage(context.Background(), storage))

	file, err := os.Open(storage.Layers[0])
	require.NoError(t, err)
	require.Error(t, checkClosedMacOSStorage(context.Background(), storage), "an open layer is refused")
	require.NoError(t, file.Close())

	require.NoError(t, os.Remove(storage.Aux))
	require.NoError(t, os.Link(storage.Overlay, storage.Aux))
	require.Error(t, checkClosedMacOSStorage(context.Background(), storage), "aliased files are refused")
	require.NoError(t, os.Remove(storage.Aux))
	require.NoError(t, os.WriteFile(storage.Aux, nil, 0600))
	require.Error(t, checkClosedMacOSStorage(context.Background(), storage), "empty files are refused")
}

func TestMacOSBuildStopAndStorageRequireBoundedInputs(t *testing.T) {
	m := &manager{}
	_, e := m.StopMacOSBuildInstance(context.Background(), "unused")
	require.Error(t, e)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, e = m.StopMacOSBuildInstance(ctx, "unused")
	require.ErrorIs(t, e, context.Canceled)
	_, e = m.MacOSBuildStorage(context.Background(), "unused")
	require.Error(t, e)
	_, e = m.MacOSBuildStorage(ctx, "unused")
	require.ErrorIs(t, e, context.Canceled)
}
