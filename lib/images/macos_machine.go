package images

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"

	"github.com/kernel/hypeman/lib/forkvm"
)

const (
	MacOSMachineVersionLabel  = "io.hypeman.machine-image.version"
	MacOSMachineKindLabel     = "io.hypeman.machine-image.kind"
	MacOSMachineDiskLabel     = "io.hypeman.machine-image.disk-path"
	MacOSMachineFormatLabel   = "io.hypeman.machine-image.disk-format"
	MacOSMachineAuxLabel      = "io.hypeman.machine-image.aux-path"
	MacOSMachinePlatformLabel = "io.hypeman.machine-image.platform-path"
)

// installMacOSMachine installs a macOS machine image's auxiliary storage and records
// its platform on meta. It returns the paths installed, for rollback. Rootfs images
// install nothing here.
func installMacOSMachine(layout imageLayout, staged stagedImageFiles, meta *imageMetadata, actualPlatform Platform) ([]string, error) {
	if actualPlatform.OS == "darwin" && staged.macos == nil {
		return nil, fmt.Errorf("macOS image requires a validated machine bundle")
	}
	if staged.macos == nil {
		return nil, nil
	}
	auxPath := filepath.Join(layout.dir, "aux.img")
	if err := os.Rename(staged.aux, auxPath); err != nil {
		return nil, err
	}
	meta.MacOS = staged.macos
	return []string{auxPath}, nil
}

// checkCachedMachinePlatform refuses a cached macOS record that does not match an
// explicit platform, so it answers the same way an uncached pull would.
func checkCachedMachinePlatform(requested *Platform, meta *imageMetadata) error {
	if requested != nil && (requested.OS == "darwin" || meta.MacOS != nil) && meta.Status == StatusReady && meta.Platform != "" {
		actual, err := ParsePlatform(meta.Platform)
		if err != nil || !requested.Matches(actual) {
			return fmt.Errorf("%w: requested %s but cached manifest is %s", ErrInvalidPlatform, *requested, meta.Platform)
		}
	}
	return nil
}

// stageMacOSMachine copies a validated bundle's boot disk and auxiliary storage to
// build-private paths, outside the manager lock. It returns the bytes both files
// occupy, which is the size recorded for accounting.
func stageMacOSMachine(payload *macOSMachinePayload, diskTemp, auxTemp string) (stagedImageFiles, error) {
	diskSize, err := stageMachineFile(payload.Disk, diskTemp)
	if err != nil {
		return stagedImageFiles{}, fmt.Errorf("stage boot disk: %w", err)
	}
	auxSize, err := stageMachineFile(payload.Aux, auxTemp)
	if err != nil {
		return stagedImageFiles{}, fmt.Errorf("stage auxiliary storage: %w", err)
	}
	return stagedImageFiles{disk: diskTemp, aux: auxTemp, macos: payload.Platform, sizeBytes: diskSize + auxSize}, nil
}

func stageMachineFile(src, dst string) (int64, error) {
	// Copy helpers create the destination with the registry-supplied source mode,
	// and a multi-GB copy would expose contents before any later chmod. The copy
	// happens in installAtomically's fresh 0700 directory and is made 0600 before
	// it appears at the canonical staging path.
	err := installAtomically(dst, func(path string) error {
		if err := forkvm.CopyRegularFile(src, path); err != nil {
			return err
		}
		return os.Chmod(path, 0600)
	})
	if err != nil {
		return 0, err
	}
	info, err := os.Stat(dst)
	if err != nil {
		return 0, err
	}
	return info.Size(), nil
}

// macOSMachineHost is true where macOS machine images can boot. Other hosts
// refuse to pull them. Tests override it.
var macOSMachineHost = runtime.GOOS == "darwin"

// checkMachinePlatformHost refuses an explicit darwin pull on a host that cannot
// boot it, before any registry round-trip.
func checkMachinePlatformHost(requested Platform) error {
	if requested.OS == "darwin" && !macOSMachineHost {
		return fmt.Errorf("%w: macOS machine images can only be pulled on a macOS host", ErrInvalidPlatform)
	}
	return nil
}

// checkMacOSMachineMetadata validates darwin labels from the config blob, so
// malformed artifacts fail before layer unpacking. Non-darwin returns false.
func checkMacOSMachineMetadata(meta *containerMetadata) (bool, error) {
	// Normalize as resolveManifestPlatform does, so aliases such as aarch64 and
	// case variants such as Darwin select the machine path rather than rootfs.
	normalized := Platform{OS: meta.OS, Architecture: meta.Architecture, Variant: meta.Variant}.Normalize()
	if normalized.OS != "darwin" {
		return false, nil
	}
	if err := checkMachinePlatformHost(normalized); err != nil {
		return true, err
	}
	if normalized.Architecture != "arm64" || normalized.Variant != "" {
		return true, fmt.Errorf("macOS machine requires darwin/arm64")
	}
	labels := meta.Labels
	if labels[MacOSMachineVersionLabel] != "1" || labels[MacOSMachineKindLabel] != "macos-image" || labels[MacOSMachineFormatLabel] != "raw" {
		return true, fmt.Errorf("darwin artifact requires version 1 macos-image with raw disk, not an ordinary container image")
	}
	// The files themselves are checked after unpack; their names can be checked now.
	paths := []string{labels[MacOSMachineDiskLabel], labels[MacOSMachineAuxLabel], labels[MacOSMachinePlatformLabel]}
	for i, path := range paths {
		if path == "" || !filepath.IsLocal(path) {
			return true, fmt.Errorf("darwin artifact requires local disk, auxiliary storage and platform config paths")
		}
		for _, other := range paths[:i] {
			if filepath.Clean(path) == filepath.Clean(other) {
				return true, fmt.Errorf("darwin artifact disk, auxiliary storage and platform config paths must be distinct")
			}
		}
	}
	return true, nil
}

func parseMacOSMachine(root string, meta *containerMetadata) (*macOSMachinePayload, error) {
	darwin, err := checkMacOSMachineMetadata(meta)
	if err != nil || !darwin {
		return nil, err
	}
	labels := meta.Labels
	return readMacOSMachineBundle(root, labels[MacOSMachineDiskLabel], labels[MacOSMachineAuxLabel], labels[MacOSMachinePlatformLabel], false)
}
