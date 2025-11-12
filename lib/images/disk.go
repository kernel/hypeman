package images

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/u-root/u-root/pkg/cpio"
)

// ExportFormat defines supported rootfs export formats
type ExportFormat string

const (
	FormatExt4  ExportFormat = "ext4"  // Read-only ext4 (app images, default)
	FormatErofs ExportFormat = "erofs" // Read-only compressed (future: when kernel supports it)
	FormatCpio  ExportFormat = "cpio"  // Uncompressed archive (initrd, fast boot)
)

// DefaultImageFormat is the default export format for OCI images
const DefaultImageFormat = FormatExt4

// ExportRootfs exports rootfs directory in specified format (public for system manager)
func ExportRootfs(rootfsDir, outputPath string, format ExportFormat) (int64, error) {
	switch format {
	case FormatExt4:
		return convertToExt4(rootfsDir, outputPath)
	case FormatErofs:
		return convertToErofs(rootfsDir, outputPath)
	case FormatCpio:
		return convertToCpio(rootfsDir, outputPath)
	default:
		return 0, fmt.Errorf("unsupported export format: %s", format)
	}
}

// convertToCpio packages directory as uncompressed cpio archive (initramfs format)
// Uses uncompressed format for faster boot (kernel loads directly without decompression)
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

	// Create newc format cpio writer (kernel-compatible format)
	cpioWriter := cpio.Newc.Writer(outFile)

	// Create recorder for tracking inodes and device numbers
	recorder := cpio.NewRecorder()

	// Walk the rootfs directory and add all files
	err = filepath.Walk(rootfsDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		// Get path relative to rootfs root
		relPath, err := filepath.Rel(rootfsDir, path)
		if err != nil {
			return err
		}

		// Skip the root directory itself
		if relPath == "." {
			return nil
		}

		// Get cpio record from file
		rec, err := recorder.GetRecord(path)
		if err != nil {
			return fmt.Errorf("get cpio record for %s: %w", path, err)
		}

		// Set the name to be relative to root
		rec.Name = relPath

		// Write the record to the archive
		if err := cpioWriter.WriteRecord(rec); err != nil {
			return fmt.Errorf("write cpio record for %s: %w", path, err)
		}

		return nil
	})

	if err != nil {
		return 0, fmt.Errorf("walk rootfs: %w", err)
	}

	// Write CPIO trailer (required to mark end of archive)
	if err := cpio.WriteTrailer(cpioWriter); err != nil {
		return 0, fmt.Errorf("write cpio trailer: %w", err)
	}

	// Get file size
	stat, err := os.Stat(outputPath)
	if err != nil {
		return 0, fmt.Errorf("stat output: %w", err)
	}

	return stat.Size(), nil
}

// convertToExt4 converts a rootfs directory to an ext4 disk image using mkfs.ext4
func convertToExt4(rootfsDir, diskPath string) (int64, error) {
	// Calculate size of rootfs directory
	sizeBytes, err := dirSize(rootfsDir)
	if err != nil {
		return 0, fmt.Errorf("calculate dir size: %w", err)
	}

	// Add 20% overhead for filesystem metadata, minimum 10MB
	diskSizeBytes := sizeBytes + (sizeBytes / 5)
	const minSize = 10 * 1024 * 1024 // 10MB
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
	// -b 4096: 4KB blocks (standard, matches VM page size)
	// -O ^has_journal: Disable journal (not needed for read-only VM mounts)
	// -d: Copy directory contents into filesystem
	// -F: Force creation (file not block device)
	cmd := exec.Command("mkfs.ext4", "-b", "4096", "-O", "^has_journal", "-d", rootfsDir, "-F", diskPath)
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

