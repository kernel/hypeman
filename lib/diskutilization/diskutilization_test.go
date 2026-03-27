package diskutilization

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/kernel/hypeman/lib/paths"
	"github.com/stretchr/testify/require"
)

type sparseWrite struct {
	offset int64
	data   []byte
}

func TestCollect_UsesAllocatedBytesAndClassifiesSnapshots(t *testing.T) {
	p := paths.New(t.TempDir())

	imagePath := filepath.Join(p.ImagesDir(), "library", "sha256:abc", "rootfs.erofs")
	require.NoError(t, createSparseTestFile(imagePath, 32*1024*1024, []sparseWrite{
		{offset: 0, data: bytes.Repeat([]byte("i"), 4096)},
	}))

	ociBlobPath := filepath.Join(p.SystemOCICache(), "blobs", "sha256", "blob")
	require.NoError(t, createSparseTestFile(ociBlobPath, 16*1024, []sparseWrite{
		{offset: 0, data: bytes.Repeat([]byte("o"), 8192)},
	}))

	volumePath := p.VolumeData("vol-1")
	require.NoError(t, createSparseTestFile(volumePath, 64*1024*1024, []sparseWrite{
		{offset: 0, data: bytes.Repeat([]byte("v"), 4096)},
	}))

	rootfsOverlayPath := p.InstanceOverlay("inst-1")
	require.NoError(t, createSparseTestFile(rootfsOverlayPath, 64*1024*1024, []sparseWrite{
		{offset: 4 * 1024 * 1024, data: bytes.Repeat([]byte("r"), 4096)},
	}))

	volumeOverlayPath := p.InstanceVolumeOverlay("inst-1", "vol-1")
	require.NoError(t, createSparseTestFile(volumeOverlayPath, 64*1024*1024, []sparseWrite{
		{offset: 8 * 1024 * 1024, data: bytes.Repeat([]byte("x"), 4096)},
	}))

	uncompressedSnapshotDir := p.InstanceSnapshotLatest("inst-1")
	require.NoError(t, createSparseTestFile(filepath.Join(uncompressedSnapshotDir, "memory-ranges"), 64*1024*1024, []sparseWrite{
		{offset: 0, data: bytes.Repeat([]byte("m"), 4096)},
	}))
	require.NoError(t, os.WriteFile(filepath.Join(uncompressedSnapshotDir, "config.json"), []byte(`{}`), 0644))

	compressedSnapshotDir := p.InstanceSnapshotLatest("inst-2")
	require.NoError(t, createSparseTestFile(filepath.Join(compressedSnapshotDir, "memory-ranges.zst"), 32*1024*1024, []sparseWrite{
		{offset: 0, data: bytes.Repeat([]byte("c"), 4096)},
	}))
	require.NoError(t, os.WriteFile(filepath.Join(compressedSnapshotDir, "config.json"), []byte(`{}`), 0644))

	otherSnapshotDir := p.InstanceSnapshotLatest("inst-3")
	require.NoError(t, os.MkdirAll(otherSnapshotDir, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(otherSnapshotDir, "config.json"), []byte(`{}`), 0644))

	utilization, err := Collect(p)
	require.NoError(t, err)

	require.Equal(t, allocatedBytesForPath(imagePath), utilization.Images)
	require.Equal(t, allocatedBytesForPath(volumePath), utilization.Volumes)
	require.Equal(t, allocatedBytesForPath(rootfsOverlayPath), utilization.RootfsOverlays)
	require.Equal(t, allocatedBytesForPath(volumeOverlayPath), utilization.VolumeOverlays)

	require.Less(t, utilization.Volumes, fileSize(t, volumePath))
	require.Less(t, utilization.RootfsOverlays, fileSize(t, rootfsOverlayPath))

	uncompressedTotal, err := sumTreeAllocatedBytes(uncompressedSnapshotDir)
	require.NoError(t, err)
	compressedTotal, err := sumTreeAllocatedBytes(compressedSnapshotDir)
	require.NoError(t, err)
	otherTotal, err := sumTreeAllocatedBytes(otherSnapshotDir)
	require.NoError(t, err)

	require.Equal(t, uncompressedTotal, utilization.SnapshotUncompressed)
	require.Equal(t, compressedTotal, utilization.SnapshotCompressed)
	require.Equal(t, otherTotal, utilization.SnapshotOther)
}

func createSparseTestFile(path string, size int64, writes []sparseWrite) error {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}

	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()

	if err := f.Truncate(size); err != nil {
		return err
	}

	for _, write := range writes {
		if _, err := f.WriteAt(write.data, write.offset); err != nil {
			return err
		}
	}

	return f.Sync()
}

func fileSize(t *testing.T, path string) int64 {
	t.Helper()

	info, err := os.Stat(path)
	require.NoError(t, err)
	return info.Size()
}
