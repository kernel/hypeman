package images

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

// convertToExt4 converts a rootfs directory to an ext4 disk image using mkfs.ext4
func convertToExt4(rootfsDir, diskPath string) (int64, error) {
	// Calculate size of rootfs directory
	sizeBytes, err := dirSize(rootfsDir)
	if err != nil {
		return 0, fmt.Errorf("calculate dir size: %w", err)
	}

	// Add 20% overhead for filesystem metadata, minimum 1GB
	diskSizeBytes := sizeBytes + (sizeBytes / 5)
	const minSize = 1024 * 1024 * 1024 // 1GB
	if diskSizeBytes < minSize {
		diskSizeBytes = minSize
	}

	// Ensure parent directory exists
	if err := os.MkdirAll(filepath.Dir(diskPath), 0755); err != nil {
		return 0, fmt.Errorf("create disk parent dir: %w", err)
	}

	// Create sparse file
	f, err := os.Create(diskPath)
	if err != nil {
		return 0, fmt.Errorf("create disk file: %w", err)
	}
	if err := f.Truncate(diskSizeBytes); err != nil {
		f.Close()
		return 0, fmt.Errorf("truncate disk file: %w", err)
	}
	f.Close()

	// Format as ext4 with rootfs contents using mkfs.ext4
	// This works without root when creating filesystem in a regular file
	cmd := exec.Command("mkfs.ext4", "-d", rootfsDir, "-F", diskPath)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return 0, fmt.Errorf("mkfs.ext4 failed: %w, output: %s", err, output)
	}

	// Get actual disk size
	stat, err := os.Stat(diskPath)
	if err != nil {
		return 0, fmt.Errorf("stat disk: %w", err)
	}

	return stat.Size(), nil
}

// dirSize calculates the total size of a directory
func dirSize(path string) (int64, error) {
	var size int64
	err := filepath.Walk(path, func(_ string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() {
			size += info.Size()
		}
		return nil
	})
	return size, err
}

