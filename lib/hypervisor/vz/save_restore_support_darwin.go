//go:build darwin

package vz

import (
	"runtime"

	"golang.org/x/sys/unix"
)

// macOSProductVersion returns the host macOS product version (e.g. "14.5.1").
// It is a variable so tests can stub the probe.
var macOSProductVersion = func() string {
	v, err := unix.Sysctl("kern.osproductversion")
	if err != nil {
		return ""
	}
	return v
}

// saveRestoreSupported reports whether this host supports VZ VM save/restore.
// Detection is runtime-derived (Apple Silicon + macOS 14+) so static
// capabilities never overstate snapshot/standby support on older macOS.
func saveRestoreSupported() bool {
	return SaveRestoreSupported(runtime.GOOS, runtime.GOARCH, macOSProductVersion())
}
