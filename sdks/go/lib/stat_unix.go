//go:build unix

package lib

import (
	"io/fs"
	"syscall"
)

// getFileOwnership returns the UID and GID of a file on Unix systems.
func getFileOwnership(info fs.FileInfo) (uid, gid uint32) {
	if stat, ok := info.Sys().(*syscall.Stat_t); ok {
		return stat.Uid, stat.Gid
	}
	return 0, 0
}
