package instances

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/kernel/hypeman/lib/paths"
	"github.com/stretchr/testify/require"
)

func TestInstanceStorageRejectsPathTraversal(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), "data")
	p := paths.New(dataDir)
	require.NoError(t, os.MkdirAll(dataDir, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(dataDir, "metadata.json"), []byte(`{"id":"outside"}`), 0644))
	marker := filepath.Join(dataDir, "keep")
	require.NoError(t, os.WriteFile(marker, []byte("keep"), 0644))
	mgr := &manager{paths: p}

	_, err := mgr.loadMetadata("..")
	require.ErrorIs(t, err, ErrNotFound)
	require.ErrorIs(t, mgr.saveMetadata(&metadata{StoredMetadata: StoredMetadata{Id: ".."}}), paths.ErrInvalidPathComponent)
	require.ErrorIs(t, mgr.deleteInstanceData(".."), ErrNotFound)
	require.FileExists(t, marker)
}
