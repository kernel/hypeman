//go:build linux

package forkvm

import (
	"errors"
	"fmt"
	"io/fs"
	"os"

	"golang.org/x/sys/unix"
)

// copyRegularFileReflink attempts to clone srcPath to dstPath via FICLONE
// (reflink). On filesystems that support copy-on-write at the block layer
// (btrfs, xfs with reflink=1, zfs, bcachefs), this is effectively
// instantaneous and consumes no additional space until pages diverge.
//
// Returns ErrReflinkUnsupported when the filesystem or kernel rejects the
// operation; callers should fall back to a full-copy path.
func copyRegularFileReflink(srcPath, dstPath string, perms fs.FileMode) (retErr error) {
	src, err := os.Open(srcPath)
	if err != nil {
		return err
	}
	defer src.Close()

	dst, err := os.OpenFile(dstPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, perms)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := dst.Close(); retErr == nil && cerr != nil {
			retErr = cerr
		}
		if retErr != nil {
			_ = os.Remove(dstPath)
		}
	}()

	if err := unix.IoctlFileClone(int(dst.Fd()), int(src.Fd())); err != nil {
		if isReflinkUnsupportedError(err) {
			return fmt.Errorf("%w: FICLONE rejected for %s: %v", ErrReflinkUnsupported, srcPath, err)
		}
		return fmt.Errorf("FICLONE %s -> %s: %w", srcPath, dstPath, err)
	}
	return nil
}

// isReflinkUnsupportedError returns true when an FICLONE failure indicates the
// operation cannot be served by the filesystem and the caller should fall
// back. Real errors (EIO, ENOSPC) propagate as-is.
func isReflinkUnsupportedError(err error) bool {
	switch {
	case errors.Is(err, unix.EINVAL),
		errors.Is(err, unix.ENOTSUP),
		errors.Is(err, unix.EOPNOTSUPP),
		errors.Is(err, unix.EXDEV),
		errors.Is(err, unix.ETXTBSY),
		errors.Is(err, unix.EISDIR),
		errors.Is(err, unix.ENOTTY):
		return true
	}
	return false
}
