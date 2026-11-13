package machineimage

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/kernel/hypeman/lib/images"
)

// Tart's OCI media types and annotations for stacked images.
const (
	tartDiskMediaType        = "application/vnd.cirruslabs.tart.disk.v2"
	tartOverlayMediaType     = "application/vnd.cirruslabs.tart.disk.asif.overlay.v1"
	tartFileDigestAnnotation = "org.cirruslabs.tart.disk-file-content-digest"
)

type tartConfig struct {
	OS            string `json:"os"`
	Arch          string `json:"arch"`
	ECID          []byte `json:"ecid"`
	HardwareModel []byte `json:"hardwareModel"`
	CPUCount      uint   `json:"cpuCount"`
	MemorySize    uint64 `json:"memorySize"`
	MACAddress    string `json:"macAddress"`
	DiskFormat    string `json:"diskFormat"`
}

type tartManifest struct {
	Layers []struct {
		MediaType   string            `json:"mediaType"`
		Annotations map[string]string `json:"annotations"`
	} `json:"layers"`
}

// ReadTartVM returns the storage of a stopped Tart macOS VM directory, for
// publishing as a machine image. A standalone VM's disk.img must be ASIF and
// becomes a one-layer image. A stacked VM's read-only layers, base first, come
// from Tart's content store, read from its manifest.json, with its writable
// overlay.asif on top; the content store is under TART_HOME, or two levels above
// the VM directory by default. The platform keeps Tart's identity; the publisher
// gives each image a fresh one.
func ReadTartVM(dir string, guestAgent bool) (Storage, error) {
	var config tartConfig
	data, err := os.ReadFile(filepath.Join(dir, "config.json"))
	if err != nil {
		return Storage{}, err
	}
	if err := json.Unmarshal(data, &config); err != nil {
		return Storage{}, fmt.Errorf("tart config: %w", err)
	}
	if config.OS != "darwin" || config.Arch != "arm64" {
		return Storage{}, fmt.Errorf("tart VM is %s/%s, want darwin/arm64", config.OS, config.Arch)
	}
	if _, err := os.Lstat(filepath.Join(dir, "state.vzvmsave")); err == nil {
		return Storage{}, fmt.Errorf("tart VM is suspended; shut it down from the guest first")
	}
	storage := Storage{
		Aux: filepath.Join(dir, "nvram.bin"),
		Platform: images.MacOSImage{
			HardwareModel:     config.HardwareModel,
			MachineIdentifier: config.ECID,
			MAC:               config.MACAddress,
			CPUs:              config.CPUCount,
			Memory:            config.MemorySize,
			GuestAgent:        guestAgent,
		},
	}
	manifest := filepath.Join(dir, "manifest.json")
	overlay := filepath.Join(dir, "overlay.asif")
	if _, err := os.Lstat(overlay); err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return Storage{}, err
		}
		if config.DiskFormat != "asif" {
			return Storage{}, fmt.Errorf("tart VM disk format is %q, want asif", config.DiskFormat)
		}
		storage.Overlay = filepath.Join(dir, "disk.img")
		return storage, nil
	}
	storage.Overlay = overlay
	home := os.Getenv("TART_HOME")
	if home == "" {
		home = filepath.Dir(filepath.Dir(dir))
	}
	storage.Layers, storage.LayerDigests, err = tartStackLayers(manifest, filepath.Join(home, "cache", "content"))
	return storage, err
}

// tartStackLayers returns the content-store paths and file digests of a stacked
// VM's read-only layers, base first: the whole-file digest on the first chunk of
// the base and of each overlay. Tart names content files by that digest.
func tartStackLayers(path, contentDir string) ([]string, []string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, err
	}
	var manifest tartManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return nil, nil, fmt.Errorf("tart manifest: %w", err)
	}
	var layers, digests []string
	for i, layer := range manifest.Layers {
		if layer.MediaType != tartDiskMediaType && layer.MediaType != tartOverlayMediaType {
			continue
		}
		digest, ok := layer.Annotations[tartFileDigestAnnotation]
		if !ok {
			continue
		}
		hex, found := strings.CutPrefix(digest, "sha256:")
		if !found || !validDigest(digest) {
			return nil, nil, fmt.Errorf("tart manifest layer %d has an invalid disk digest", i)
		}
		if len(layers) == 0 && layer.MediaType != tartDiskMediaType {
			return nil, nil, fmt.Errorf("tart manifest disk layers must start with the base")
		}
		layers = append(layers, filepath.Join(contentDir, "sha256", hex))
		digests = append(digests, digest)
	}
	if len(layers) == 0 {
		return nil, nil, fmt.Errorf("tart manifest names no stacked disk files")
	}
	return layers, digests, nil
}
