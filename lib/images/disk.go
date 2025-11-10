package images

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

// convertToErofs converts a rootfs directory to an erofs disk image using mkfs.erofs
func convertToErofs(rootfsDir, diskPath string) (int64, error) {
	// Ensure parent directory exists
	if err := os.MkdirAll(filepath.Dir(diskPath), 0755); err != nil {
		return 0, fmt.Errorf("create disk parent dir: %w", err)
	}

	// Create erofs image with LZ4 fast compression
	// -zlz4: LZ4 fast compression (~20-25% space savings, faster builds)
	// erofs doesn't need pre-allocation, creates file directly
	cmd := exec.Command("mkfs.erofs", "-zlz4", diskPath, rootfsDir)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return 0, fmt.Errorf("mkfs.erofs failed: %w, output: %s", err, output)
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

