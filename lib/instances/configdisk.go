package instances

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/onkernel/hypeman/lib/devices"
	"github.com/onkernel/hypeman/lib/images"
	"github.com/onkernel/hypeman/lib/network"
	"github.com/onkernel/hypeman/lib/vmconfig"
)

// createConfigDisk generates an ext4 disk with instance configuration.
// The disk contains /config.json read by the guest init binary.
func (m *manager) createConfigDisk(ctx context.Context, inst *Instance, imageInfo *images.Image, netConfig *network.NetworkConfig) error {
	// Create temporary directory for config files
	tmpDir, err := os.MkdirTemp("", "hypeman-config-*")
	if err != nil {
		return fmt.Errorf("create temp dir: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	// Generate config.json
	cfg := m.buildGuestConfig(ctx, inst, imageInfo, netConfig)
	configData, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}
	configPath := filepath.Join(tmpDir, "config.json")
	if err := os.WriteFile(configPath, configData, 0644); err != nil {
		return fmt.Errorf("write config.json: %w", err)
	}

	// Create ext4 disk with config files
	diskPath := m.paths.InstanceConfigDisk(inst.Id)
	_, err = images.ExportRootfs(tmpDir, diskPath, images.FormatExt4)
	if err != nil {
		return fmt.Errorf("create config disk: %w", err)
	}

	return nil
}

// buildGuestConfig creates the vmconfig.Config struct for the guest init binary.
func (m *manager) buildGuestConfig(ctx context.Context, inst *Instance, imageInfo *images.Image, netConfig *network.NetworkConfig) *vmconfig.Config {
	cfg := &vmconfig.Config{
		Entrypoint: imageInfo.Entrypoint,
		Cmd:        imageInfo.Cmd,
		Workdir:    imageInfo.WorkingDir,
		Env:        mergeEnv(imageInfo.Env, inst.Env),
		InitMode:   "exec",
	}

	if cfg.Workdir == "" {
		cfg.Workdir = "/"
	}

	// Network configuration
	if inst.NetworkEnabled && netConfig != nil {
		cfg.NetworkEnabled = true
		cfg.GuestIP = netConfig.IP
		cfg.GuestCIDR = netmaskToCIDR(netConfig.Netmask)
		cfg.GuestGW = netConfig.Gateway
		cfg.GuestDNS = netConfig.DNS
	}

	// GPU passthrough - check if any attached device is a GPU
	for _, deviceID := range inst.Devices {
		device, err := m.deviceManager.GetDevice(ctx, deviceID)
		if err == nil && device.Type == devices.DeviceTypeGPU {
			cfg.HasGPU = true
			break
		}
	}

	// Volume mounts
	// Volumes are attached as /dev/vdd, /dev/vde, etc. (after vda=rootfs, vdb=overlay, vdc=config)
	deviceIdx := 0
	for _, vol := range inst.Volumes {
		device := fmt.Sprintf("/dev/vd%c", 'd'+deviceIdx)
		mount := vmconfig.VolumeMount{
			Device: device,
			Path:   vol.MountPath,
		}
		if vol.Overlay {
			mount.Mode = "overlay"
			mount.OverlayDevice = fmt.Sprintf("/dev/vd%c", 'd'+deviceIdx+1)
			deviceIdx += 2
		} else {
			if vol.Readonly {
				mount.Mode = "ro"
			} else {
				mount.Mode = "rw"
			}
			deviceIdx++
		}
		cfg.VolumeMounts = append(cfg.VolumeMounts, mount)
	}

	// Determine init mode based on image CMD
	if images.IsSystemdImage(imageInfo.Entrypoint, imageInfo.Cmd) {
		cfg.InitMode = "systemd"
	}

	return cfg
}

// mergeEnv merges image environment variables with instance overrides.
func mergeEnv(imageEnv map[string]string, instEnv map[string]string) map[string]string {
	result := make(map[string]string)

	// Start with image env
	for k, v := range imageEnv {
		result[k] = v
	}

	// Override with instance env
	for k, v := range instEnv {
		result[k] = v
	}

	return result
}

// netmaskToCIDR converts dotted decimal netmask to CIDR prefix length.
// e.g., "255.255.255.0" -> 24, "255.255.0.0" -> 16
func netmaskToCIDR(netmask string) int {
	parts := strings.Split(netmask, ".")
	if len(parts) != 4 {
		return 24 // default to /24
	}
	bits := 0
	for _, p := range parts {
		n, _ := strconv.Atoi(p)
		for n > 0 {
			bits += n & 1
			n >>= 1
		}
	}
	return bits
}
