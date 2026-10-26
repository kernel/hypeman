package instances

import (
	"testing"

	"github.com/kernel/hypeman/lib/images"
	"github.com/stretchr/testify/require"
)

func TestVMStoragePathOnlyForMacOSGuests(t *testing.T) {
	require.Empty(t, (&StoredMetadata{DataDir: "/data/guests/i1"}).VMStoragePath())
	mac := &StoredMetadata{DataDir: "/data/guests/i1", MacOS: &images.MacOSImage{}}
	require.Equal(t, "/data/guests/i1/boot.raw", mac.VMStoragePath())
	require.Empty(t, (&StoredMetadata{MacOS: &images.MacOSImage{}}).VMStoragePath(), "no data dir, no path")
}
