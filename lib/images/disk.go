package images

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

// ExportFormat defines supported rootfs export formats
type ExportFormat string

const (
	FormatErofs ExportFormat = "erofs" // Read-only compressed (app images)
	FormatCpio  ExportFormat = "cpio"  // Compressed archive (initrd)
)

// ExportRootfs exports rootfs directory in specified format (public for system manager)
func ExportRootfs(rootfsDir, outputPath string, format ExportFormat) (int64, error) {
	switch format {
	case FormatErofs:
		return convertToErofs(rootfsDir, outputPath)
	case FormatCpio:
		return convertToCpio(rootfsDir, outputPath)
	default:
		return 0, fmt.Errorf("unsupported export format: %s", format)
	}
}

// convertToCpio packages directory as gzipped cpio archive (initramfs format)
func convertToCpio(rootfsDir, outputPath string) (int64, error) {
	// Ensure parent directory exists
	if err := os.MkdirAll(filepath.Dir(outputPath), 0755); err != nil {
		return 0, fmt.Errorf("create output dir: %w", err)
	}

	// Create output file
	outFile, err := os.Create(outputPath)
	if err != nil {
		return 0, fmt.Errorf("create output file: %w", err)
	}
	defer outFile.Close()

	// Pipeline: (cd rootfsDir && find . -print0 | cpio -H newc -o --null | gzip -9) > output
	findCmd := exec.Command("find", ".", "-print0")
	findCmd.Dir = rootfsDir

	cpioCmd := exec.Command("cpio", "-H", "newc", "-o", "--null", "--quiet")
	cpioCmd.Dir = rootfsDir

	gzipCmd := exec.Command("gzip", "-9")

	// Connect pipes
	cpioPipe, err := cpioCmd.StdinPipe()
	if err != nil {
		return 0, err
	}
	gzipPipe, err := gzipCmd.StdinPipe()
	if err != nil {
		return 0, err
	}

	findCmd.Stdout = cpioPipe
	cpioCmd.Stdout = gzipPipe
	gzipCmd.Stdout = outFile

	// Start all commands in reverse order
	if err := gzipCmd.Start(); err != nil {
		return 0, fmt.Errorf("start gzip: %w", err)
	}
	if err := cpioCmd.Start(); err != nil {
		return 0, fmt.Errorf("start cpio: %w", err)
	}
	if err := findCmd.Start(); err != nil {
		return 0, fmt.Errorf("start find: %w", err)
	}

	// Wait for find to complete
	if err := findCmd.Wait(); err != nil {
		return 0, fmt.Errorf("find failed: %w", err)
	}
	cpioPipe.Close()

	// Wait for cpio
	if err := cpioCmd.Wait(); err != nil {
		return 0, fmt.Errorf("cpio failed: %w", err)
	}
	gzipPipe.Close()

	// Wait for gzip
	if err := gzipCmd.Wait(); err != nil {
		return 0, fmt.Errorf("gzip failed: %w", err)
	}

	// Get file size
	stat, err := os.Stat(outputPath)
	if err != nil {
		return 0, fmt.Errorf("stat output: %w", err)
	}

	return stat.Size(), nil
}

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

