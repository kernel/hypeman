//go:build linux

package qemu

import (
	"os"
	"syscall"
)

// fileChangeIdentity returns the inode and change time of a file. Unlike the
// modification time, neither can be restored after the file is rewritten.
func fileChangeIdentity(info os.FileInfo) (inode uint64, changedNanos int64) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0
	}
	return stat.Ino, stat.Ctim.Nano()
}
