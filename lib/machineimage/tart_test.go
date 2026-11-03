package machineimage

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/Code-Hex/vz/v4/diskimagekit"
	"github.com/stretchr/testify/require"
)

// tartTestVM writes a Tart VM directory under home/vms/name with config.json and
// nvram.bin, and returns its path.
func tartTestVM(t *testing.T, home, name string, config map[string]any) string {
	t.Helper()
	dir := filepath.Join(home, "vms", name)
	require.NoError(t, os.MkdirAll(dir, 0700))
	base := map[string]any{"version": 1, "os": "darwin", "arch": "arm64", "ecid": []byte{2}, "hardwareModel": []byte{1},
		"cpuCountMin": 4, "cpuCount": 4, "memorySizeMin": 8 << 30, "memorySize": 8 << 30, "macAddress": "02:00:00:00:00:01", "diskFormat": "asif"}
	for k, v := range config {
		base[k] = v
	}
	data, err := json.Marshal(base)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "config.json"), data, 0600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "nvram.bin"), []byte("nvram"), 0600))
	return dir
}

func TestReadTartVMStandalone(t *testing.T) {
	dir := tartTestVM(t, t.TempDir(), "base", nil)
	img, err := diskimagekit.CreateASIF(filepath.Join(dir, "disk.img"), (4<<20)/512)
	require.NoError(t, err)
	require.NoError(t, img.Close())

	storage, err := ReadTartVM(dir, true)
	require.NoError(t, err)
	require.Empty(t, storage.Layers)
	require.Equal(t, filepath.Join(dir, "disk.img"), storage.Overlay)
	require.Equal(t, filepath.Join(dir, "nvram.bin"), storage.Aux)
	require.Equal(t, []byte{1}, storage.Platform.HardwareModel)
	require.Equal(t, []byte{2}, storage.Platform.MachineIdentifier)
	require.Equal(t, uint(4), storage.Platform.CPUs)
	require.True(t, storage.Platform.GuestAgent)
	require.NoError(t, storage.validate())

	raw := tartTestVM(t, t.TempDir(), "raw", map[string]any{"diskFormat": "raw"})
	_, err = ReadTartVM(raw, false)
	require.ErrorContains(t, err, "want asif")

	require.NoError(t, os.WriteFile(filepath.Join(dir, "state.vzvmsave"), []byte("x"), 0600))
	_, err = ReadTartVM(dir, false)
	require.ErrorContains(t, err, "suspended")

	linux := tartTestVM(t, t.TempDir(), "linux", map[string]any{"os": "linux"})
	_, err = ReadTartVM(linux, false)
	require.ErrorContains(t, err, "want darwin/arm64")
}

func TestReadTartVMStacked(t *testing.T) {
	home := t.TempDir()
	content := filepath.Join(home, "cache", "content", "sha256")
	require.NoError(t, os.MkdirAll(content, 0700))
	// A base and one immutable overlay in the content store, named by digest.
	digest := func(name string) string {
		sum := sha256.Sum256([]byte(name))
		return hex.EncodeToString(sum[:])
	}
	basePath, layerPath := filepath.Join(content, digest("base")), filepath.Join(content, digest("layer"))
	img, err := diskimagekit.CreateASIF(basePath, (4<<20)/512)
	require.NoError(t, err)
	require.NoError(t, img.CreateLayer(layerPath, diskimagekit.Overlay))

	dir := tartTestVM(t, home, "build", nil)
	require.NoError(t, img.CreateLayer(filepath.Join(dir, "overlay.asif"), diskimagekit.Overlay))
	require.NoError(t, img.Close())
	disk := func(media, digest string) map[string]any {
		layer := map[string]any{"mediaType": media}
		if digest != "" {
			layer["annotations"] = map[string]string{tartFileDigestAnnotation: "sha256:" + digest}
		}
		return layer
	}
	writeManifest := func(layers ...map[string]any) {
		all := append([]map[string]any{{"mediaType": "application/vnd.cirruslabs.tart.config.v1"}}, layers...)
		all = append(all, map[string]any{"mediaType": "application/vnd.cirruslabs.tart.nvram.v1"})
		data, err := json.Marshal(map[string]any{"layers": all})
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(filepath.Join(dir, "manifest.json"), data, 0600))
	}
	writeManifest(disk(tartDiskMediaType, digest("base")), disk(tartDiskMediaType, ""), disk(tartOverlayMediaType, digest("layer")))

	storage, err := ReadTartVM(dir, false)
	require.NoError(t, err)
	require.Equal(t, []string{basePath, layerPath}, storage.Layers)
	require.Equal(t, filepath.Join(dir, "overlay.asif"), storage.Overlay)
	require.NoError(t, storage.validate())

	writeManifest(disk(tartOverlayMediaType, digest("layer")))
	_, err = ReadTartVM(dir, false)
	require.ErrorContains(t, err, "must start with the base")

	writeManifest(disk(tartDiskMediaType, ""))
	_, err = ReadTartVM(dir, false)
	require.ErrorContains(t, err, "names no stacked disk files")

	// A layer missing from the content store fails validation.
	writeManifest(disk(tartDiskMediaType, digest("missing")))
	storage, err = ReadTartVM(dir, false)
	require.NoError(t, err)
	require.Error(t, storage.validate())
}
