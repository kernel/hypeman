//go:build darwin

// Package shimconfig defines the configuration types shared between
// the hypeman API server and the vz-shim subprocess.
package shimconfig

// ShimConfig is the configuration passed from hypeman to the shim.
type ShimConfig struct {
	// Compute resources
	VCPUs       int   `json:"vcpus"`
	MemoryBytes int64 `json:"memory_bytes"`

	// Storage
	Disks []DiskConfig `json:"disks"`

	// Network
	Networks []NetworkConfig `json:"networks"`

	// Console
	SerialLogPath string `json:"serial_log_path"`

	// Boot configuration
	KernelPath string `json:"kernel_path"`
	InitrdPath string `json:"initrd_path"`
	KernelArgs string `json:"kernel_args"`

	// Socket paths (where shim should listen)
	ControlSocket string `json:"control_socket"`
	VsockSocket   string `json:"vsock_socket"`

	// Logging
	LogPath string `json:"log_path"`
}

// DiskConfig represents a disk attached to the VM.
type DiskConfig struct {
	Path     string `json:"path"`
	Readonly bool   `json:"readonly"`
}

// NetworkConfig represents a network interface.
type NetworkConfig struct {
	MAC string `json:"mac"`
}
