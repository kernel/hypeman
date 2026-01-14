//go:build windows

package lib

import "io/fs"

// getFileOwnership returns 0, 0 on Windows as UID/GID are Unix concepts.
func getFileOwnership(info fs.FileInfo) (uid, gid uint32) {
	return 0, 0
}
