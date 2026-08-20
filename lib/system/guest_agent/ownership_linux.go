//go:build linux

package main

import (
	"os"
	"syscall"
)

func setFileOwnership(path string, uid, gid uint32) error {
	return os.Chown(path, int(uid), int(gid))
}

func fileOwnership(info os.FileInfo) (uint32, uint32) {
	if stat, ok := info.Sys().(*syscall.Stat_t); ok {
		return stat.Uid, stat.Gid
	}
	return 0, 0
}
