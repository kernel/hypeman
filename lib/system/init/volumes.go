package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/onkernel/hypeman/lib/vmconfig"
)

// mountVolumes mounts attached volumes according to the configuration.
// Supports three modes: ro (read-only), rw (read-write), and overlay.
func mountVolumes(log *Logger, cfg *vmconfig.Config) error {
	log.Info("volumes", "mounting volumes")

	for _, vol := range cfg.VolumeMounts {
		mountPath := filepath.Join("/overlay/newroot", vol.Path)

		// Create mount point
		if err := os.MkdirAll(mountPath, 0755); err != nil {
			log.Error("volumes", fmt.Sprintf("mkdir %s failed", vol.Path), err)
			continue
		}

		switch vol.Mode {
		case "overlay":
			if err := mountVolumeOverlay(log, vol, mountPath); err != nil {
				log.Error("volumes", fmt.Sprintf("mount overlay %s failed", vol.Path), err)
			}
		case "ro":
			if err := mountVolumeReadOnly(log, vol, mountPath); err != nil {
				log.Error("volumes", fmt.Sprintf("mount ro %s failed", vol.Path), err)
			}
		default: // "rw"
			if err := mountVolumeReadWrite(log, vol, mountPath); err != nil {
				log.Error("volumes", fmt.Sprintf("mount rw %s failed", vol.Path), err)
			}
		}
	}

	return nil
}

// mountVolumeOverlay mounts a volume in overlay mode.
// Uses the base device as read-only lower layer and overlay device for writable upper layer.
func mountVolumeOverlay(log *Logger, vol vmconfig.VolumeMount, mountPath string) error {
	// Use device name for unique mount points (e.g., "vdd" from "/dev/vdd")
	// This avoids collisions when multiple volumes have the same basename
	deviceName := filepath.Base(vol.Device)
	baseMount := fmt.Sprintf("/mnt/vol-base-%s", deviceName)
	overlayMount := fmt.Sprintf("/mnt/vol-overlay-%s", deviceName)

	// Create mount points
	if err := os.MkdirAll(baseMount, 0755); err != nil {
		return err
	}
	if err := os.MkdirAll(overlayMount, 0755); err != nil {
		return err
	}

	// Mount base volume read-only (noload to skip journal recovery)
	cmd := exec.Command("/bin/mount", "-t", "ext4", "-o", "ro,noload", vol.Device, baseMount)
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("mount base: %s: %s", err, output)
	}

	// Mount overlay disk (writable)
	cmd = exec.Command("/bin/mount", "-t", "ext4", vol.OverlayDevice, overlayMount)
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("mount overlay disk: %s: %s", err, output)
	}

	// Create overlay directories
	upperDir := filepath.Join(overlayMount, "upper")
	workDir := filepath.Join(overlayMount, "work")
	os.MkdirAll(upperDir, 0755)
	os.MkdirAll(workDir, 0755)

	// Create overlayfs
	options := fmt.Sprintf("lowerdir=%s,upperdir=%s,workdir=%s", baseMount, upperDir, workDir)
	cmd = exec.Command("/bin/mount", "-t", "overlay", "-o", options, "overlay", mountPath)
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("mount overlay: %s: %s", err, output)
	}

	log.Info("volumes", fmt.Sprintf("mounted %s at %s (overlay via %s)", vol.Device, vol.Path, vol.OverlayDevice))
	return nil
}

// mountVolumeReadOnly mounts a volume in read-only mode.
func mountVolumeReadOnly(log *Logger, vol vmconfig.VolumeMount, mountPath string) error {
	// Use noload to skip journal recovery for multi-attach safety
	cmd := exec.Command("/bin/mount", "-t", "ext4", "-o", "ro,noload", vol.Device, mountPath)
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%s: %s", err, output)
	}

	log.Info("volumes", fmt.Sprintf("mounted %s at %s (ro)", vol.Device, vol.Path))
	return nil
}

// mountVolumeReadWrite mounts a volume in read-write mode.
func mountVolumeReadWrite(log *Logger, vol vmconfig.VolumeMount, mountPath string) error {
	cmd := exec.Command("/bin/mount", "-t", "ext4", vol.Device, mountPath)
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%s: %s", err, output)
	}

	log.Info("volumes", fmt.Sprintf("mounted %s at %s (rw)", vol.Device, vol.Path))
	return nil
}

