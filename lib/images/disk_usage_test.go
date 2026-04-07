package images

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestTotalReadyImageBytesFromMetadata_UsesRootfsFallbackForMalformedMetadata(t *testing.T) {
	t.Parallel()

	imagesDir := t.TempDir()
	digestDir := filepath.Join(imagesDir, "docker.io", "library", "alpine", "sha256deadbeef")
	require.NoError(t, os.MkdirAll(digestDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(digestDir, "metadata.json"), []byte("{not-json"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(digestDir, "rootfs.erofs"), []byte("rootfs-data"), 0o644))

	total, err := totalReadyImageBytesFromMetadata(imagesDir)
	require.NoError(t, err)
	require.Equal(t, int64(len("rootfs-data")), total)
}

func TestTotalReadyImageBytesFromMetadata_UsesRootfsFallbackForReadyImageWithoutSize(t *testing.T) {
	t.Parallel()

	imagesDir := t.TempDir()
	digestDir := filepath.Join(imagesDir, "docker.io", "library", "alpine", "sha256deadbeef")
	require.NoError(t, os.MkdirAll(digestDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(digestDir, "metadata.json"), []byte(`{"status":"ready","size_bytes":0}`), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(digestDir, "rootfs.erofs"), []byte("another-rootfs"), 0o644))

	total, err := totalReadyImageBytesFromMetadata(imagesDir)
	require.NoError(t, err)
	require.Equal(t, int64(len("another-rootfs")), total)
}
