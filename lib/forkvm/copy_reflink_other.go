//go:build !linux && !darwin

package forkvm

import (
	"fmt"
	"io/fs"
)

// copyRegularFileReflink is unavailable on platforms without a copy-on-write
// clone primitive; callers fall back to the sparse copy.
func copyRegularFileReflink(srcPath, dstPath string, perms fs.FileMode) error {
	_ = dstPath
	_ = perms
	return fmt.Errorf("%w: reflink unsupported on this platform: %s", ErrReflinkUnsupported, srcPath)
}
