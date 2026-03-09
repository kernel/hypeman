//go:build !darwin && !linux

package forkvm

import (
	"fmt"
	"io/fs"
)

func copyRegularFileSparse(srcPath, dstPath string, perms fs.FileMode) error {
	_ = dstPath
	_ = perms
	return fmt.Errorf("%w: unsupported platform for sparse copy: %s", ErrSparseCopyUnsupported, srcPath)
}
