//go:build darwin

package resources

import (
	"runtime"
)

// detectCPUCapacity returns the number of logical CPUs on macOS.
// Uses runtime.NumCPU() which calls sysctl on macOS.
func detectCPUCapacity() (int64, error) {
	return int64(runtime.NumCPU()), nil
}
