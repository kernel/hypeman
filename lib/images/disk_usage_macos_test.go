package images

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// writeMacOSImageForUsage writes a ready macOS image whose chain files hold 1 MiB
// of data in a 64 MiB logical disk each, with diffIDs for its layers.
func writeMacOSImageForUsage(t *testing.T, imagesDir, digest string, diffIDs []string) {
	t.Helper()
	dir := filepath.Join(imagesDir, "content", digest)
	require.NoError(t, os.MkdirAll(dir, 0700))
	for i := range diffIDs {
		file, err := os.Create(filepath.Join(dir, MacOSDiskLayerFile(i)))
		require.NoError(t, err)
		_, err = file.Write(make([]byte, 1<<20))
		require.NoError(t, err)
		require.NoError(t, file.Truncate(64<<20))
		require.NoError(t, file.Close())
	}
	require.NoError(t, os.WriteFile(filepath.Join(dir, MacOSMachineAuxFile), []byte("aux"), 0600))
	data, err := json.Marshal(imageMetadata{Name: "builds/" + digest, Digest: "sha256:" + digest, Status: StatusReady, MacOS: &MacOSImage{}, MacOSDiskLayers: len(diffIDs), MacOSDiskDiffIDs: diffIDs, SizeBytes: 64 << 20})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "metadata.json"), data, 0600))
}

func TestMacOSImageDiskUsageCountsAllocatedBytesAndSharedLayersOnce(t *testing.T) {
	imagesDir := t.TempDir()
	writeMacOSImageForUsage(t, imagesDir, "aaaa", []string{"sha256:base"})
	writeMacOSImageForUsage(t, imagesDir, "bbbb", []string{"sha256:base", "sha256:build"})

	total, err := totalReadyImageBytesFromMetadataWithContext(context.Background(), imagesDir)
	require.NoError(t, err)
	// Two distinct layers of about 1 MiB each, not three 64 MiB logical disks.
	require.Greater(t, total, int64(2<<20)-1)
	require.Less(t, total, int64(4<<20))
}
