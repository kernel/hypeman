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
)

var (
	// ErrArchiveTooLarge is returned when extracted content exceeds the size limit
	ErrArchiveTooLarge = errors.New("archive content exceeds size limit")
	// ErrInvalidArchivePath is returned when a tar entry has a malicious path
	ErrInvalidArchivePath = errors.New("invalid archive path")
)

// @sjmiller609 todo: do we have a dependency we can use for safe extraction?
// ExtractTarGz extracts a tar.gz archive to destDir, aborting if the extracted
// content exceeds maxBytes. Returns the total extracted bytes on success.
//
// Safety measures against adversarial archives:
// - Tracks cumulative extracted size, aborts immediately if limit exceeded
// - Validates paths to prevent directory traversal attacks
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

		// Validate and sanitize path
		targetPath, err := sanitizePath(destDir, header.Name)
		if err != nil {
			return extractedBytes, err
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
			// Validate symlink target doesn't escape destDir
			linkTarget := header.Linkname
			if filepath.IsAbs(linkTarget) {
				return extractedBytes, fmt.Errorf("%w: absolute symlink target", ErrInvalidArchivePath)
			}

			// Resolve the symlink relative to its location
			resolvedTarget := filepath.Join(filepath.Dir(targetPath), linkTarget)
			resolvedTarget = filepath.Clean(resolvedTarget)

			// Ensure resolved path is within destDir
			if !strings.HasPrefix(resolvedTarget, filepath.Clean(destDir)+string(os.PathSeparator)) &&
				resolvedTarget != filepath.Clean(destDir) {
				return extractedBytes, fmt.Errorf("%w: symlink escapes destination", ErrInvalidArchivePath)
			}

			// Ensure parent directory exists
			if err := os.MkdirAll(filepath.Dir(targetPath), 0755); err != nil {
				return extractedBytes, fmt.Errorf("create parent dir for symlink: %w", err)
			}

			if err := os.Symlink(linkTarget, targetPath); err != nil {
				return extractedBytes, fmt.Errorf("create symlink %s: %w", header.Name, err)
			}

		case tar.TypeLink:
			// Hard links - validate target is within destDir
			linkTarget, err := sanitizePath(destDir, header.Linkname)
			if err != nil {
				return extractedBytes, err
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

// sanitizePath validates and returns a safe path within destDir
func sanitizePath(destDir, name string) (string, error) {
	// Clean the path
	name = filepath.Clean(name)

	// Reject absolute paths
	if filepath.IsAbs(name) {
		return "", fmt.Errorf("%w: absolute path %s", ErrInvalidArchivePath, name)
	}

	// Reject paths with ..
	if strings.Contains(name, "..") {
		return "", fmt.Errorf("%w: path traversal in %s", ErrInvalidArchivePath, name)
	}

	// Build target path
	targetPath := filepath.Join(destDir, name)

	// Double-check the result is within destDir
	if !strings.HasPrefix(filepath.Clean(targetPath), filepath.Clean(destDir)+string(os.PathSeparator)) &&
		filepath.Clean(targetPath) != filepath.Clean(destDir) {
		return "", fmt.Errorf("%w: path escapes destination: %s", ErrInvalidArchivePath, name)
	}

	return targetPath, nil
}

