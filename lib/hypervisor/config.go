package hypervisor

// VMConfig is the hypervisor-agnostic VM configuration.
// Each hypervisor implementation translates this to its native format.
type VMConfig struct {
	// Compute resources
	VCPUs        int
	MemoryBytes  int64
	HotplugBytes int64
	Topology     *CPUTopology
	GuestMemory  GuestMemoryConfig

	// Storage
	Disks []DiskConfig

	// Network
	Networks []NetworkConfig

	// Console
	SerialLogPath string

	// Vsock
	VsockCID    int64
	VsockSocket string

	// PCI device passthrough (GPU, etc.)
	PCIDevices     []string
	VGPUDevicePath string

	// Boot configuration. Empty BootMode preserves the existing direct-kernel
	// behavior for Linux callers.
	BootMode BootMode
	Firmware *FirmwareConfig
	TPM      *TPMConfig

	KernelPath string
	InitrdPath string
	KernelArgs string

	// EnableRosetta attaches an Apple Rosetta virtio-fs share (vz on Apple
	// silicon only) so the guest can execute x86-64 binaries.
	EnableRosetta bool
}

// GuestMemoryConfig contains hypervisor-agnostic guest memory feature toggles.
type GuestMemoryConfig struct {
	EnableBalloon     bool
	FreePageReporting bool
	DeflateOnOOM      bool
	FreePageHinting   bool
	// RequireBalloon controls whether VM startup should fail if balloon setup fails.
	RequireBalloon bool
}

// CPUTopology defines the virtual CPU topology
type CPUTopology struct {
	ThreadsPerCore int
	CoresPerDie    int
	DiesPerPackage int
	Packages       int
}

type BootMode string

const (
	BootModeDirect BootMode = "direct"
	BootModeUEFI   BootMode = "uefi"
)

// EffectiveBootMode preserves direct Linux kernel boot for existing callers.
func (c VMConfig) EffectiveBootMode() BootMode {
	if c.BootMode == "" {
		return BootModeDirect
	}
	return c.BootMode
}

// FirmwareConfig describes UEFI firmware files. CodePath is immutable firmware;
// VarsPath is per-instance writable variable storage.
type FirmwareConfig struct {
	CodePath   string
	VarsPath   string
	SecureBoot bool
}

// TPMConfig describes a per-instance software TPM 2.0 endpoint.
type TPMConfig struct {
	SocketPath string
	StateDir   string
}

type DiskFormat string

const (
	DiskFormatRaw   DiskFormat = "raw"
	DiskFormatQCOW2 DiskFormat = "qcow2"
)

// EffectiveFormat preserves raw disks for existing callers.
func (d DiskConfig) EffectiveFormat() DiskFormat {
	if d.Format == "" {
		return DiskFormatRaw
	}
	return d.Format
}

// DiskConfig represents a disk attached to the VM
type DiskConfig struct {
	Path       string
	Format     DiskFormat
	Readonly   bool
	IOBps      int64 // Sustained I/O rate limit in bytes/sec (0 = unlimited)
	IOBurstBps int64 // Burst I/O rate in bytes/sec (0 = same as IOBps)
}

// NetworkConfig represents a network interface attached to the VM
type NetworkConfig struct {
	TAPDevice string
	IP        string
	MAC       string
	Netmask   string
	// DownloadBps limits host->guest bandwidth in bytes/sec (0 = unlimited).
	// Hypeman enforces this host-side via TAP shaping for all hypervisors.
	// Firecracker also maps it to per-interface API rate limiters.
	DownloadBps int64
	// UploadBps limits guest->host bandwidth in bytes/sec (0 = unlimited).
	// Hypeman enforces this host-side via TAP shaping for all hypervisors.
	// Firecracker also maps it to per-interface API rate limiters.
	UploadBps int64
}

// VMInfo contains current VM state information
type VMInfo struct {
	State            VMState
	MemoryActualSize *int64 // Current actual memory size in bytes (if available)
}

// VMState represents the VM execution state
type VMState string

const (
	// StateCreated means the VM is configured but not running
	StateCreated VMState = "created"
	// StateRunning means the VM is actively executing
	StateRunning VMState = "running"
	// StatePaused means the VM execution is suspended
	StatePaused VMState = "paused"
	// StateShutdown means the VM has stopped but VMM exists
	StateShutdown VMState = "shutdown"
)
