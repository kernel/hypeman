package images

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestStageMachineFileNeverKeepsRegistryMode(t *testing.T) {
	root := t.TempDir()
	src := filepath.Join(root, "source.raw")
	require.NoError(t, os.WriteFile(src, []byte("synthetic"), 0644))
	require.NoError(t, os.Chmod(src, 0644))
	dst := filepath.Join(root, "staged.raw")
	size, err := stageMachineFile(src, dst)
	require.NoError(t, err)
	require.EqualValues(t, len("synthetic"), size)
	info, err := os.Stat(dst)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0600), info.Mode().Perm())
	entries, err := os.ReadDir(root)
	require.NoError(t, err)
	for _, entry := range entries {
		require.NotContains(t, entry.Name(), ".install-", "private staging directory leaked")
	}
}
