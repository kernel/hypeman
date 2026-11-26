package images

import (
	"context"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestExportRootfsWithContextMissingErofsTool(t *testing.T) {
	// Isolate command lookup even when mkfs.erofs is installed on the host.
	t.Setenv("PATH", t.TempDir())
	rootfsDir := t.TempDir()
	diskPath := filepath.Join(t.TempDir(), "rootfs.erofs")

	_, err := ExportRootfsWithContext(context.Background(), rootfsDir, diskPath, FormatErofs)

	require.ErrorIs(t, err, exec.ErrNotFound)
	require.Contains(t, err.Error(), "mkfs.erofs")
	require.Contains(t, err.Error(), "sudo apt-get install erofs-utils")
	require.NoFileExists(t, diskPath)
}
