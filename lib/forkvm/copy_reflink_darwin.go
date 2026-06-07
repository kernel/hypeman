//go:build darwin

package forkvm

import (
	"errors"
	"fmt"
	"io/fs"
	"os"

	"golang.org/x/sys/unix"
)

// copyRegularFileReflink clones srcPath to dstPath via clonefile(2) (APFS
// copy-on-write). On APFS this is effectively instantaneous and consumes no
// additional space until the cloned file's pages diverge.
//
// Unlike the Linux FICLONE path, clonefile(2) requires the destination to NOT
// exist: it creates the destination as part of the clone. We remove any stale
// destination first and must not pre-create or O_TRUNC it.
//
// Returns ErrReflinkUnsupported when the volume cannot serve the clone (e.g.
// cross-volume EXDEV, non-APFS ENOTSUP); callers fall back to a sparse copy.
// Real errors (EIO, ENOSPC, EACCES) propagate as-is.
func copyRegularFileReflink(srcPath, dstPath string, perms fs.FileMode) error {
	// clonefile fails with EEXIST if the destination is present. The walk may be
	// re-running over a partially populated destination, so clear it first.
	if err := os.Remove(dstPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove stale clone destination %s: %w", dstPath, err)
	}

	// CLONE_NOFOLLOW clones the file at srcPath itself, never a symlink target.
	// CLONE_NOOWNERCOPY drops owner/SUID/SGID/ACL metadata we have no business
	// propagating into a fork; the explicit Chmod below re-establishes the mode.
	flags := unix.CLONE_NOFOLLOW | unix.CLONE_NOOWNERCOPY
	if err := unix.Clonefile(srcPath, dstPath, flags); err != nil {
		if isReflinkUnsupportedError(err) {
			return fmt.Errorf("%w: clonefile rejected for %s: %v", ErrReflinkUnsupported, srcPath, err)
		}
		return fmt.Errorf("clonefile %s -> %s: %w", srcPath, dstPath, err)
	}

	if err := os.Chmod(dstPath, perms); err != nil {
		_ = os.Remove(dstPath)
		return fmt.Errorf("chmod cloned file %s: %w", dstPath, err)
	}
	return nil
}

// isReflinkUnsupportedError returns true when a clonefile failure indicates the
// clone cannot be served and the caller should fall back to a sparse copy.
// Real errors (EIO, ENOSPC, EACCES) return false and propagate.
func isReflinkUnsupportedError(err error) bool {
	switch {
	case errors.Is(err, unix.ENOTSUP),
		errors.Is(err, unix.EOPNOTSUPP),
		errors.Is(err, unix.EXDEV),
		errors.Is(err, unix.EEXIST),
		errors.Is(err, unix.EINVAL),
		errors.Is(err, unix.ENOTDIR):
		return true
	}
	return false
}
