package devices

// HostCapabilities describes optional device features supported by the host platform.
type HostCapabilities struct {
	// SupportsVGPU indicates whether the platform supports mediated vGPU operations.
	SupportsVGPU bool
}
