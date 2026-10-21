package instances

import (
	"path/filepath"
	"testing"

	"github.com/kernel/hypeman/lib/images"
	"github.com/kernel/hypeman/lib/paths"
	"github.com/stretchr/testify/require"
)

func TestVMStoragePathIsTheMacOSBootDisk(t *testing.T) {
	t.Parallel()
	dataDir := t.TempDir()
	stored := &StoredMetadata{MacOS: &images.MacOSImage{}, DataDir: dataDir}
	require.Equal(t, filepath.Join(dataDir, paths.MacOSBootDiskFile), stored.VMStoragePath())
}

func TestVMStoragePathIsEmptyForLinuxGuests(t *testing.T) {
	t.Parallel()
	stored := &StoredMetadata{DataDir: t.TempDir()}
	require.Empty(t, stored.VMStoragePath())
}
