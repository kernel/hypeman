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

func TestTotalReadyImageBytesFromMetadata_DeduplicatesHardLinkedAliases(t *testing.T) {
	t.Parallel()

	imagesDir := t.TempDir()
	sourceDir := filepath.Join(imagesDir, "source", "digest")
	targetDir := filepath.Join(imagesDir, "target", "digest")
	require.NoError(t, os.MkdirAll(sourceDir, 0o755))
	require.NoError(t, os.MkdirAll(targetDir, 0o755))

	sourceRootfs := filepath.Join(sourceDir, "rootfs.erofs")
	targetRootfs := filepath.Join(targetDir, "rootfs.erofs")
	require.NoError(t, os.WriteFile(sourceRootfs, []byte("shared-rootfs"), 0o644))
	require.NoError(t, os.Link(sourceRootfs, targetRootfs))
	metadata := []byte(`{"status":"ready","size_bytes":13}`)
	require.NoError(t, os.WriteFile(filepath.Join(sourceDir, "metadata.json"), metadata, 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(targetDir, "metadata.json"), metadata, 0o644))

	total, err := totalReadyImageBytesFromMetadata(imagesDir)
	require.NoError(t, err)
	require.Equal(t, int64(len("shared-rootfs")), total)
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
