//go:build !windows

package hypervisor

import (
	"fmt"
	"os"
	"syscall"
)

// SocketCacheKey returns a cache key that changes when a Unix socket path is
// recreated, preventing stale state from being reused across VM restarts.
func SocketCacheKey(socketPath string) string {
	info, err := os.Stat(socketPath)
	if err != nil {
		return socketPath
	}
	if stat, ok := info.Sys().(*syscall.Stat_t); ok {
		return fmt.Sprintf("%s:%d:%d", socketPath, stat.Dev, stat.Ino)
	}
	return socketPath
}
