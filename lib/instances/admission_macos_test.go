package instances

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/kernel/hypeman/lib/images"
	"github.com/kernel/hypeman/lib/paths"
	"github.com/stretchr/testify/require"
)

func TestMacOSDiskReservationIsTheAllowanceNotTheImageSize(t *testing.T) {
	dir := t.TempDir()
	stored := &StoredMetadata{DataDir: dir, OverlaySize: 64 << 30, MacOS: &images.MacOSImage{}}
	require.Equal(t, int64(macOSBootDiskAllowance), storedDiskReservationBytes(stored), "a stored image-sized OverlaySize is not reserved")

	require.NoError(t, os.WriteFile(filepath.Join(dir, paths.MacOSBootDiskFile), make([]byte, 1<<20), 0600))
	require.Equal(t, int64(macOSBootDiskAllowance), storedDiskReservationBytes(stored), "a boot overlay within the allowance reserves the allowance")

	linux := &StoredMetadata{OverlaySize: 64 << 30}
	require.Equal(t, int64(64<<30), storedDiskReservationBytes(linux))
}
