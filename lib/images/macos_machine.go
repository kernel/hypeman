package images

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

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

// macOSMachinePayload is a complete cold-boot bundle, not a container rootfs.
// This spike intentionally has no base/delta format or fork identity rebinding.
type macOSMachinePayload struct {
	Disk     string
	Aux      string
	Platform *MacOSImage
}

func machineBundleFile(root, relative string) (string, error) {
	if relative == "" || !filepath.IsLocal(relative) {
		return "", fmt.Errorf("machine payload path must be local and relative")
	}
	root, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", err
	}
	path, err := filepath.EvalSymlinks(filepath.Join(root, relative))
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(root, path)
	if err != nil || !filepath.IsLocal(rel) {
		return "", fmt.Errorf("machine payload escapes artifact root")
	}
	info, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() || info.Size() == 0 {
		return "", fmt.Errorf("machine payload must be a nonempty regular file")
	}
	return path, nil
}

func sameFile(a, b string) bool {
	ai, errA := os.Stat(a)
	bi, errB := os.Stat(b)
	return errA == nil && errB == nil && os.SameFile(ai, bi)
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
	disk, err := machineBundleFile(root, labels[MacOSMachineDiskLabel])
	if err != nil {
		return nil, err
	}
	aux, err := machineBundleFile(root, labels[MacOSMachineAuxLabel])
	if err != nil {
		return nil, err
	}
	config, err := machineBundleFile(root, labels[MacOSMachinePlatformLabel])
	if err != nil {
		return nil, err
	}
	// Compare inodes: hardlinks with different names must not satisfy distinctness.
	if sameFile(disk, aux) || sameFile(disk, config) || sameFile(aux, config) {
		return nil, fmt.Errorf("machine bundle files must be distinct")
	}
	info, err := os.Stat(config)
	if err != nil {
		return nil, err
	}
	if info.Size() > 64<<10 {
		return nil, fmt.Errorf("machine platform metadata is too large")
	}
	b, err := os.ReadFile(config)
	if err != nil {
		return nil, err
	}
	var platform MacOSImage
	if err := json.Unmarshal(b, &platform); err != nil {
		return nil, err
	}
	if err := platform.Validate(); err != nil {
		return nil, err
	}
	return &macOSMachinePayload{Disk: disk, Aux: aux, Platform: &platform}, nil
}
