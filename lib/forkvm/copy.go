package forkvm

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
)

var (
	ErrSparseCopyUnsupported = errors.New("sparse copy unsupported")
	ErrReflinkUnsupported    = errors.New("reflink unsupported")
)

// reflinkDisabled, when nonzero, forces CopyGuestDirectory to skip the FICLONE
// fast path entirely. Tests set this; production code leaves it untouched.
var reflinkDisabled atomic.Bool

// SetReflinkDisabled toggles the FICLONE fast path. Intended for tests that
// need to exercise the sparse-copy fallback explicitly.
func SetReflinkDisabled(disabled bool) {
	reflinkDisabled.Store(disabled)
}

// reflinkUnsupportedSticky tracks whether reflink has already been observed to
// fail with an "unsupported" signal for this destination filesystem. Once set,
// we skip subsequent FICLONE attempts within the same CopyGuestDirectory call
// to avoid re-paying the rejection on every file.
type copyState struct {
	reflinkDead bool
}

// CopyOptions controls which guest-directory files are copied.
type CopyOptions struct {
	SkipRelativePaths map[string]struct{}
}

// CopyGuestDirectory recursively copies a guest directory to a new destination.
// Regular files are cloned via reflink (FICLONE) when the underlying filesystem
// supports it; otherwise we fall back to a sparse extent copy
// (SEEK_DATA/SEEK_HOLE). Runtime sockets and logs are skipped because they are
// host-runtime artifacts.
func CopyGuestDirectory(srcDir, dstDir string) error {
	return CopyGuestDirectoryWithOptions(srcDir, dstDir, CopyOptions{})
}

// CopyGuestDirectoryWithOptions is CopyGuestDirectory with optional path skips.
func CopyGuestDirectoryWithOptions(srcDir, dstDir string, opts CopyOptions) error {
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

	state := &copyState{}
	if reflinkDisabled.Load() {
		state.reflinkDead = true
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
		if _, ok := opts.SkipRelativePaths[filepath.Clean(relPath)]; ok {
			return nil
		}
		if d.IsDir() && shouldSkipDirectory(relPath) {
			return filepath.SkipDir
		}
		if !d.IsDir() && shouldSkipRegularFile(relPath) {
			return nil
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
			if err := copyRegularFile(state, path, dstPath, mode.Perm()); err != nil {
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

// CopyRegularFile copies one regular file using the same reflink-first behavior
// as CopyGuestDirectory.
func CopyRegularFile(srcPath, dstPath string) error {
	info, err := os.Stat(srcPath)
	if err != nil {
		return fmt.Errorf("stat source file: %w", err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("source path is not a regular file: %s", srcPath)
	}
	if err := os.MkdirAll(filepath.Dir(dstPath), 0755); err != nil {
		return fmt.Errorf("create destination parent: %w", err)
	}
	state := &copyState{}
	if reflinkDisabled.Load() {
		state.reflinkDead = true
	}
	return copyRegularFile(state, srcPath, dstPath, info.Mode().Perm())
}

// copyRegularFile clones path to dstPath, preferring FICLONE reflink and
// falling back to sparse extent copy. The state object lets us short-circuit
// future reflink attempts once we observe an "unsupported" signal from the
// destination filesystem in the current copy.
func copyRegularFile(state *copyState, srcPath, dstPath string, perms fs.FileMode) error {
	if state == nil || !state.reflinkDead {
		err := copyRegularFileReflink(srcPath, dstPath, perms)
		if err == nil {
			return nil
		}
		if !errors.Is(err, ErrReflinkUnsupported) {
			return err
		}
		if state != nil {
			state.reflinkDead = true
		}
	}
	return copyRegularFileSparse(srcPath, dstPath, perms)
}

func shouldSkipDirectory(relPath string) bool {
	return relPath == "logs"
}

func shouldSkipRegularFile(relPath string) bool {
	return strings.HasSuffix(relPath, ".lz4.tmp") || strings.HasSuffix(relPath, ".zst.tmp")
}
