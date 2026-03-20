package system

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestReplaceSymlinkAtomic(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	linkPath := filepath.Join(tmpDir, "latest")

	require.NoError(t, replaceSymlinkAtomic(linkPath, "first"))

	target, err := os.Readlink(linkPath)
	require.NoError(t, err)
	require.Equal(t, "first", target)

	require.NoError(t, replaceSymlinkAtomic(linkPath, "second"))

	target, err = os.Readlink(linkPath)
	require.NoError(t, err)
	require.Equal(t, "second", target)
}
