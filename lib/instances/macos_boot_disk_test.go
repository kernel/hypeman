package instances

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/kernel/hypeman/lib/images"
	"github.com/kernel/hypeman/lib/paths"
	"github.com/stretchr/testify/require"
)

// Instances created before the boot.raw name keep their disk at overlay.raw. Start
// and VM-process lookup must find that disk, and new instances must use boot.raw.
func TestMacOSBootDiskPathFallsBackToLegacyOverlay(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		create []string // files to create, relative to the instance dir
		want   string   // expected disk, relative to the instance dir
	}{
		{name: "new instance without a disk yet", want: paths.MacOSBootDiskFile},
		{name: "new instance", create: []string{paths.MacOSBootDiskFile}, want: paths.MacOSBootDiskFile},
		{name: "legacy instance", create: []string{"overlay.raw"}, want: "overlay.raw"},
		{name: "boot.raw wins when both exist", create: []string{paths.MacOSBootDiskFile, "overlay.raw"}, want: paths.MacOSBootDiskFile},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dataDir := t.TempDir()
			p := paths.New(dataDir)
			const id = "inst1"
			instDir := p.InstanceDir(id)
			require.NoError(t, os.MkdirAll(instDir, 0o755))
			for _, f := range tc.create {
				require.NoError(t, os.WriteFile(filepath.Join(instDir, f), []byte("disk"), 0o600))
			}
			want := filepath.Join(instDir, tc.want)

			require.Equal(t, want, macOSBootDiskPath(p, id))

			stored := &StoredMetadata{MacOS: &images.MacOSImage{}, DataDir: instDir}
			require.Equal(t, want, stored.VMStoragePath())
		})
	}
}

func TestVMStoragePathIsEmptyForLinuxGuests(t *testing.T) {
	t.Parallel()
	stored := &StoredMetadata{DataDir: t.TempDir()}
	require.Empty(t, stored.VMStoragePath())
}
