//go:build !linux

package qemu

import "os"

// The managed runtime only exists on Linux.
func fileChangeIdentity(os.FileInfo) (inode uint64, changedNanos int64) {
	return 0, 0
}
