//go:build darwin

// Package shimconfig defines the configuration types shared between
// the hypeman API server and the vz-shim subprocess.
package shimconfig

const (
	// SnapshotManifestFile is the metadata file stored in snapshot directories.
	// Kept as config.json to match existing snapshot path conventions.
	SnapshotManifestFile = "config.json"
	// SnapshotMachineStateFile is the serialized VM machine state filename.
	SnapshotMachineStateFile = "machine-state.vzm"
)

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

	// Generic machine identifier data representation (base64), used to keep
	// platform identity stable across save/restore.
	MachineIdentifierData string `json:"machine_identifier_data,omitempty"`

	// Optional restore source (snapshot machine state file path).
	// When set, the shim restores instead of starting from cold boot.
	RestoreMachineStatePath string `json:"restore_machine_state_path,omitempty"`
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

// SnapshotManifest is persisted in snapshot directories to allow restore.
type SnapshotManifest struct {
	Hypervisor       string     `json:"hypervisor"`
	MachineStateFile string     `json:"machine_state_file"`
	ShimConfig       ShimConfig `json:"shim_config"`
}
