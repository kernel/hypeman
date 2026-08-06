//go:build darwin

package devices

// Capabilities returns the device features supported on macOS.
func Capabilities() HostCapabilities {
	return HostCapabilities{SupportsVGPU: false}
}
