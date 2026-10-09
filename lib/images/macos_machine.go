package images

import (
	"fmt"
	"os"

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
	if err := forkvm.CopyRegularFile(src, dst); err != nil {
		return 0, err
	}
	// Registry-supplied modes must not expose the canonical files to other local users.
	if err := os.Chmod(dst, 0600); err != nil {
		return 0, err
	}
	info, err := os.Stat(dst)
	if err != nil {
		return 0, err
	}
	return info.Size(), nil
}

// ValidateMacOSBundle validates a complete installed machine bundle at the fixed
// disk.img/aux.img/config.json paths. It performs no boot, import or publication.
func ValidateMacOSBundle(root string) (*MacOSImage, error) {
	payload, err := parseMacOSMachine(root, &containerMetadata{OS: "darwin", Architecture: "arm64", Labels: map[string]string{
		MacOSMachineVersionLabel: "1", MacOSMachineKindLabel: "macos-image", MacOSMachineFormatLabel: "raw",
		MacOSMachineDiskLabel: "disk.img", MacOSMachineAuxLabel: "aux.img", MacOSMachinePlatformLabel: "config.json",
	}})
	if err != nil {
		return nil, err
	}
	return payload.Platform, nil
}

func parseMacOSMachine(root string, meta *containerMetadata) (*macOSMachinePayload, error) {
	// Normalize as resolveManifestPlatform does, so aliases such as aarch64 and
	// case variants such as Darwin select the machine path rather than rootfs.
	normalized := Platform{OS: meta.OS, Architecture: meta.Architecture, Variant: meta.Variant}.Normalize()
	if normalized.OS != "darwin" {
		return nil, nil
	}
	if normalized.Architecture != "arm64" || normalized.Variant != "" {
		return nil, fmt.Errorf("macOS machine requires darwin/arm64")
	}
	labels := meta.Labels
	if labels[MacOSMachineVersionLabel] != "1" || labels[MacOSMachineKindLabel] != "macos-image" || labels[MacOSMachineFormatLabel] != "raw" {
		return nil, fmt.Errorf("darwin artifact requires version 1 macos-image with raw disk, not an ordinary container image")
	}
	return readMacOSMachineBundle(root, labels[MacOSMachineDiskLabel], labels[MacOSMachineAuxLabel], labels[MacOSMachinePlatformLabel], false)
}
