package images

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
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

func parseMacOSMachine(root string, meta *containerMetadata) (*macOSMachinePayload, error) {
	if meta.OS != "darwin" {
		return nil, nil
	}
	if meta.Architecture != "arm64" || meta.Variant != "" {
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
	if len(platform.HardwareModel) == 0 || len(platform.MachineIdentifier) == 0 || platform.CPUs < 2 || platform.Memory < 4<<30 {
		return nil, fmt.Errorf("invalid macOS platform metadata")
	}
	// net.ParseMAC also accepts EUI-64 and InfiniBand addresses, which VZ rejects.
	if mac, err := net.ParseMAC(platform.MAC); err != nil || len(mac) != 6 {
		return nil, fmt.Errorf("invalid machine MAC %q: want a 6-byte Ethernet address", platform.MAC)
	}
	return &macOSMachinePayload{Disk: disk, Aux: aux, Platform: &platform}, nil
}
