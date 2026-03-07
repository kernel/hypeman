package forkvm

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

var ErrSparseCopyUnsupported = errors.New("sparse copy unsupported")

// CopyGuestDirectory recursively copies a guest directory to a new destination.
// Regular files are copied using sparse extent copy only (SEEK_DATA/SEEK_HOLE).
// Runtime sockets and logs are skipped because they are host-runtime artifacts.
func CopyGuestDirectory(srcDir, dstDir string) error {
	srcInfo, err := os.Stat(srcDir)
	if err != nil {
		return fmt.Errorf("stat source directory: %w", err)
	}
	if !srcInfo.IsDir() {
		return fmt.Errorf("source path is not a directory: %s", srcDir)
	}

	if err := os.MkdirAll(dstDir, srcInfo.Mode().Perm()); err != nil {
		return fmt.Errorf("create destination directory: %w", err)
	}

	return filepath.WalkDir(srcDir, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}

		relPath, err := filepath.Rel(srcDir, path)
		if err != nil {
			return fmt.Errorf("compute relative path: %w", err)
		}
		if relPath == "." {
			return nil
		}
		if d.IsDir() && shouldSkipDirectory(relPath) {
			return filepath.SkipDir
		}

		dstPath := filepath.Join(dstDir, relPath)
		info, err := d.Info()
		if err != nil {
			return fmt.Errorf("stat source entry %s: %w", path, err)
		}

		mode := info.Mode()
		switch {
		case mode.IsDir():
			if err := os.MkdirAll(dstPath, mode.Perm()); err != nil {
				return fmt.Errorf("create destination directory %s: %w", dstPath, err)
			}
			return nil

		case mode.IsRegular():
			if err := copyRegularFileSparse(path, dstPath, mode.Perm()); err != nil {
				return fmt.Errorf("copy file %s: %w", path, err)
			}
			return nil

		case mode&os.ModeSymlink != 0:
			target, err := os.Readlink(path)
			if err != nil {
				return fmt.Errorf("read symlink %s: %w", path, err)
			}
			if err := os.Symlink(target, dstPath); err != nil {
				return fmt.Errorf("create symlink %s: %w", dstPath, err)
			}
			return nil

		case mode&os.ModeSocket != 0:
			// Runtime socket; the forked instance will create its own.
			return nil

		default:
			return fmt.Errorf("unsupported file type %s (%s)", path, mode.String())
		}
	})
}

func shouldSkipDirectory(relPath string) bool {
	return relPath == "logs"
}
