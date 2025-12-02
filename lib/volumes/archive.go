package volumes

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	securejoin "github.com/cyphar/filepath-securejoin"
)

var (
	// ErrArchiveTooLarge is returned when extracted content exceeds the size limit
	ErrArchiveTooLarge = errors.New("archive content exceeds size limit")
	// ErrInvalidArchivePath is returned when a tar entry has a malicious path
	ErrInvalidArchivePath = errors.New("invalid archive path")
)

// validateArchivePath checks if a path from an archive is safe.
// We reject obviously malicious paths rather than silently sanitizing them,
// since a legitimate archive should not contain path traversal attempts.
func validateArchivePath(name string) error {
	// Clean the path first
	cleaned := filepath.Clean(name)

	// Reject absolute paths
	if filepath.IsAbs(cleaned) || filepath.IsAbs(name) {
		return fmt.Errorf("%w: absolute path %q", ErrInvalidArchivePath, name)
	}

	// Reject paths with .. components
	if strings.HasPrefix(cleaned, "..") || strings.Contains(cleaned, string(filepath.Separator)+"..") {
		return fmt.Errorf("%w: path traversal in %q", ErrInvalidArchivePath, name)
	}

	return nil
}

// ExtractTarGz extracts a tar.gz archive to destDir, aborting if the extracted
// content exceeds maxBytes. Returns the total extracted bytes on success.
//
// Safety measures against adversarial archives:
// - Rejects archives containing path traversal attempts or absolute paths
// - Tracks cumulative extracted size, aborts immediately if limit exceeded
// - Uses securejoin for safe path joining (defense in depth)
// - Uses io.LimitReader as secondary protection when copying files
func ExtractTarGz(r io.Reader, destDir string, maxBytes int64) (int64, error) {
	// Create destination directory
	if err := os.MkdirAll(destDir, 0755); err != nil {
		return 0, fmt.Errorf("create dest dir: %w", err)
	}

	// Wrap in gzip reader
	gzr, err := gzip.NewReader(r)
	if err != nil {
		return 0, fmt.Errorf("gzip reader: %w", err)
	}
	defer gzr.Close()

	// Create tar reader
	tr := tar.NewReader(gzr)

	var extractedBytes int64

	for {
		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return extractedBytes, fmt.Errorf("read tar header: %w", err)
		}

		// Validate path - reject archives with malicious entries
		if err := validateArchivePath(header.Name); err != nil {
			return extractedBytes, err
		}

		// Use securejoin for safe path joining (defense in depth)
		targetPath, err := securejoin.SecureJoin(destDir, header.Name)
		if err != nil {
			return extractedBytes, fmt.Errorf("%w: %v", ErrInvalidArchivePath, err)
		}

		// Check if adding this entry would exceed the limit
		if extractedBytes+header.Size > maxBytes {
			return extractedBytes, fmt.Errorf("%w: would exceed %d bytes", ErrArchiveTooLarge, maxBytes)
		}

		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(targetPath, os.FileMode(header.Mode)); err != nil {
				return extractedBytes, fmt.Errorf("create dir %s: %w", header.Name, err)
			}

		case tar.TypeReg:
			// Ensure parent directory exists
			if err := os.MkdirAll(filepath.Dir(targetPath), 0755); err != nil {
				return extractedBytes, fmt.Errorf("create parent dir: %w", err)
			}

			// Create file
			f, err := os.OpenFile(targetPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(header.Mode))
			if err != nil {
				return extractedBytes, fmt.Errorf("create file %s: %w", header.Name, err)
			}

			// Copy with limit as secondary protection
			remaining := maxBytes - extractedBytes
			limitedReader := io.LimitReader(tr, remaining+1) // +1 to detect overflow

			n, err := io.Copy(f, limitedReader)
			f.Close()

			if err != nil {
				return extractedBytes, fmt.Errorf("write file %s: %w", header.Name, err)
			}

			extractedBytes += n

			// Check if we hit the limit
			if extractedBytes > maxBytes {
				return extractedBytes, fmt.Errorf("%w: exceeded %d bytes", ErrArchiveTooLarge, maxBytes)
			}

		case tar.TypeSymlink:
			// Reject absolute symlink targets
			if filepath.IsAbs(header.Linkname) {
				return extractedBytes, fmt.Errorf("%w: absolute symlink target %q", ErrInvalidArchivePath, header.Linkname)
			}

			// Reject symlinks with path traversal attempts
			// We check this explicitly because securejoin sanitizes rather than errors
			cleanedLink := filepath.Clean(header.Linkname)
			if strings.HasPrefix(cleanedLink, ".."+string(filepath.Separator)) || cleanedLink == ".." {
				return extractedBytes, fmt.Errorf("%w: symlink %q escapes destination", ErrInvalidArchivePath, header.Linkname)
			}

			// Validate symlink target - resolve relative to symlink's directory
			symlinkDir := filepath.Dir(targetPath)
			resolvedTarget, err := securejoin.SecureJoin(symlinkDir, header.Linkname)
			if err != nil {
				return extractedBytes, fmt.Errorf("%w: symlink target unsafe: %v", ErrInvalidArchivePath, err)
			}

			// Verify the resolved target is within destDir (defense in depth)
			cleanDest := filepath.Clean(destDir)
			if !strings.HasPrefix(resolvedTarget, cleanDest+string(filepath.Separator)) && resolvedTarget != cleanDest {
				return extractedBytes, fmt.Errorf("%w: symlink %q escapes destination", ErrInvalidArchivePath, header.Linkname)
			}

			// Ensure parent directory exists
			if err := os.MkdirAll(filepath.Dir(targetPath), 0755); err != nil {
				return extractedBytes, fmt.Errorf("create parent dir for symlink: %w", err)
			}

			if err := os.Symlink(header.Linkname, targetPath); err != nil {
				return extractedBytes, fmt.Errorf("create symlink %s: %w", header.Name, err)
			}

		case tar.TypeLink:
			// Hard links - validate target is within destDir using securejoin
			linkTarget, err := securejoin.SecureJoin(destDir, header.Linkname)
			if err != nil {
				return extractedBytes, fmt.Errorf("%w: hardlink target unsafe: %v", ErrInvalidArchivePath, err)
			}

			// Ensure parent directory exists
			if err := os.MkdirAll(filepath.Dir(targetPath), 0755); err != nil {
				return extractedBytes, fmt.Errorf("create parent dir for hardlink: %w", err)
			}

			if err := os.Link(linkTarget, targetPath); err != nil {
				return extractedBytes, fmt.Errorf("create hardlink %s: %w", header.Name, err)
			}

		default:
			// Skip other types (devices, fifos, etc.)
			continue
		}
	}

	return extractedBytes, nil
}

