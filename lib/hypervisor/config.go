package hypervisor

// VMConfig is the hypervisor-agnostic VM configuration.
// Each hypervisor implementation translates this to its native format.
type VMConfig struct {
	// Compute resources
	VCPUs        int
	MemoryBytes  int64
	HotplugBytes int64
	Topology     *CPUTopology

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
	PCIDevices []string

	// Boot configuration
	KernelPath string
	InitrdPath string
	KernelArgs string
}

// CPUTopology defines the virtual CPU topology
type CPUTopology struct {
	ThreadsPerCore int
	CoresPerDie    int
	DiesPerPackage int
	Packages       int
}

// DiskConfig represents a disk attached to the VM
type DiskConfig struct {
	Path       string
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
