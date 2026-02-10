//go:build darwin

package resources

import (
	"golang.org/x/sys/unix"
)

// detectMemoryCapacity returns total physical memory on macOS using sysctl.
func detectMemoryCapacity() (int64, error) {
	// Use sysctl to get hw.memsize
	memsize, err := unix.SysctlUint64("hw.memsize")
	if err != nil {
		return 0, err
	}
	return int64(memsize), nil
}
