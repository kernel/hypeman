package images

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestMacOSBundleValidation(t *testing.T) {
	platform := MacOSImage{HardwareModel: []byte{1}, MachineIdentifier: []byte{2}, MAC: "02:00:00:00:00:01", CPUs: 4, Memory: 8 << 30}
	data, err := json.Marshal(platform)
	require.NoError(t, err)
	for _, name := range []string{"valid", "empty-disk", "hardlink", "escape", "oversized", "truncated", "trailing", "unknown-field", "invalid-mac"} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			require.NoError(t, os.WriteFile(filepath.Join(root, "disk.img"), []byte("disk"), 0600))
			require.NoError(t, os.WriteFile(filepath.Join(root, "aux.img"), []byte("aux"), 0600))
			config := append([]byte(nil), data...)
			switch name {
			case "empty-disk":
				require.NoError(t, os.WriteFile(filepath.Join(root, "disk.img"), nil, 0600))
			case "hardlink":
				require.NoError(t, os.Remove(filepath.Join(root, "aux.img")))
				require.NoError(t, os.Link(filepath.Join(root, "disk.img"), filepath.Join(root, "aux.img")))
			case "escape":
				outside := filepath.Join(t.TempDir(), "disk")
				require.NoError(t, os.WriteFile(outside, []byte("outside"), 0600))
				require.NoError(t, os.Remove(filepath.Join(root, "disk.img")))
				require.NoError(t, os.Symlink(outside, filepath.Join(root, "disk.img")))
			case "oversized":
				config = []byte(strings.Repeat(" ", maxMacOSMetadataBytes+1))
			case "truncated":
				config = config[:len(config)-1]
			case "trailing":
				config = append(config, []byte(" {}")...)
			case "unknown-field":
				config = append(config[:len(config)-1], []byte(",\"credential\":\"not-a-secret\"}")...)
			case "invalid-mac":
				config = []byte(strings.Replace(string(config), platform.MAC, "invalid", 1))
			}
			require.NoError(t, os.WriteFile(filepath.Join(root, "config.json"), config, 0600))
			got, err := ValidateMacOSBundle(root)
			if name == "valid" {
				require.NoError(t, err)
				require.Equal(t, &platform, got)
			} else {
				require.Error(t, err)
			}
			if name == "unknown-field" {
				bundle, err := readMacOSMachineBundle(root, "disk.img", "aux.img", "config.json", false)
				require.NoError(t, err, "import/OCI compatibility permits unknown non-export metadata")
				require.Equal(t, &platform, bundle.Platform)
			}
		})
	}
}
