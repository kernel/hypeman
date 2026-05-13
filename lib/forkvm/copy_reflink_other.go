//go:build !linux

package forkvm

import (
	"fmt"
	"io/fs"
)

// copyRegularFileReflink is unavailable on non-Linux platforms. On macOS APFS
// supports clonefile(2) and could be wired up here, but we currently only
// rely on the sparse-copy fallback off-Linux.
func copyRegularFileReflink(srcPath, dstPath string, perms fs.FileMode) error {
	_ = dstPath
	_ = perms
	return fmt.Errorf("%w: reflink unsupported on this platform: %s", ErrReflinkUnsupported, srcPath)
}
