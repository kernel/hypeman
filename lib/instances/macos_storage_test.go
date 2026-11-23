package instances

import (
	"crypto/sha256"
	"os"
	"path/filepath"
	"testing"

	"github.com/Code-Hex/vz/v4/diskimagekit"
	"github.com/kernel/hypeman/lib/images"
	"github.com/kernel/hypeman/lib/paths"
	"github.com/stretchr/testify/require"
)

// imageChainFixture writes a two-layer ASIF chain and an aux file, as an image has.
func imageChainFixture(t *testing.T) ([]string, string) {
	t.Helper()
	dir := t.TempDir()
	chain := []string{filepath.Join(dir, "disk.0.asif"), filepath.Join(dir, "disk.1.asif")}
	img, err := diskimagekit.CreateASIF(chain[0], (16<<20)/512)
	require.NoError(t, err)
	_, err = img.WriteAt([]byte("base"), 0)
	require.NoError(t, err)
	require.NoError(t, img.CreateLayer(chain[1], diskimagekit.Overlay))
	_, err = img.WriteAt([]byte("image"), 1<<20)
	require.NoError(t, err)
	require.NoError(t, img.Close())
	aux := filepath.Join(dir, "aux.img")
	require.NoError(t, os.WriteFile(aux, []byte("aux"), 0600))
	return chain, aux
}

func fileSum(t *testing.T, path string) [32]byte {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	return sha256.Sum256(data)
}

func TestCreateMacOSStorage(t *testing.T) {
	chain, aux := imageChainFixture(t)
	before := []([32]byte){fileSum(t, chain[0]), fileSum(t, chain[1])}
	dataDir := t.TempDir()
	require.NoError(t, createMacOSStorage(chain, aux, dataDir))

	stored := &StoredMetadata{DataDir: dataDir, MacOSDiskLayers: len(chain)}
	parents := macOSDiskParents(stored)
	for i, layer := range parents {
		require.Equal(t, before[i], fileSum(t, layer), "layer %d is a copy of the image's", i)
		info, err := os.Stat(layer)
		require.NoError(t, err)
		require.Equal(t, os.FileMode(0400), info.Mode().Perm(), "image layers are read-only in the instance")
	}
	overlay := filepath.Join(dataDir, paths.MacOSBootDiskFile)
	info, err := os.Stat(overlay)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0600), info.Mode().Perm())
	require.Equal(t, []byte("aux"), must(os.ReadFile(filepath.Join(dataDir, macOSAuxStorageFile))))

	// The guest's view: the image's data through the overlay, and writes land only in the overlay.
	disk, err := images.OpenMacOSDiskChain(parents)
	require.NoError(t, err)
	require.NoError(t, disk.Append(overlay, diskimagekit.WithOpenMode(diskimagekit.ReadWrite)))
	_, err = disk.WriteAt([]byte("guest"), 2<<20)
	require.NoError(t, err)
	for offset, want := range map[int64]string{0: "base", 1 << 20: "image", 2 << 20: "guest"} {
		buf := make([]byte, len(want))
		_, err := disk.ReadAt(buf, offset)
		require.NoError(t, err)
		require.Equal(t, want, string(buf))
	}
	require.NoError(t, disk.Close())
	for i, layer := range parents {
		require.Equal(t, before[i], fileSum(t, layer), "writes do not reach layer %d", i)
	}
	require.Equal(t, before[0], fileSum(t, chain[0]))
	require.Equal(t, before[1], fileSum(t, chain[1]))

	require.Error(t, createMacOSStorage(chain, aux, dataDir), "existing storage is never replaced")
	require.FileExists(t, overlay)
}

func TestCreateMacOSStorageRemovesOnlyWhatItCreated(t *testing.T) {
	chain, _ := imageChainFixture(t)
	dataDir := t.TempDir()
	existing := filepath.Join(dataDir, "unrelated")
	require.NoError(t, os.WriteFile(existing, []byte("keep"), 0600))
	require.Error(t, createMacOSStorage(chain, filepath.Join(t.TempDir(), "missing-aux.img"), dataDir))
	entries, err := os.ReadDir(dataDir)
	require.NoError(t, err)
	require.Len(t, entries, 1, "the copied layers are removed")
	require.FileExists(t, existing)
}

func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}
