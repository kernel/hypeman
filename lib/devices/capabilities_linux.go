//go:build linux

package devices

// Capabilities returns the device features supported on Linux.
func Capabilities() HostCapabilities {
	return HostCapabilities{SupportsVGPU: true}
}
