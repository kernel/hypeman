package instances

import (
	"time"

	"github.com/onkernel/hypeman/lib/vmm"
)

// State represents the instance state
type State string

const (
	StateStopped  State = "Stopped"  // No VMM, no snapshot
	StateCreated  State = "Created"  // VMM created but not booted (CH native)
	StateRunning  State = "Running"  // VM running (CH native)
	StatePaused   State = "Paused"   // VM paused (CH native)
	StateShutdown State = "Shutdown" // VM shutdown, VMM exists (CH native)
	StateStandby  State = "Standby"  // No VMM, snapshot exists
)

// Instance represents a virtual machine instance
type Instance struct {
	// Identification
	Id    string // Auto-generated ULID
	Name  string
	Image string // OCI reference

	// State
	State       State
	HasSnapshot bool

	// Resources (matching Cloud Hypervisor terminology)
	Size        int64 // Base memory in bytes
	HotplugSize int64 // Hotplug memory in bytes
	Vcpus       int

	// Configuration
	Env map[string]string

	// Timestamps
	CreatedAt time.Time
	StartedAt *time.Time
	StoppedAt *time.Time

	// Internal (not in API response, used by manager)
	KernelVersion string        // Kernel version (e.g., "ch-v6.12.9")
	InitrdVersion string        // Initrd version (e.g., "v1.0.0")
	CHVersion     vmm.CHVersion // Cloud Hypervisor version
	SocketPath    string        // Path to API socket
	DataDir       string        // Instance data directory
}

// CreateInstanceRequest is the domain request for creating an instance
type CreateInstanceRequest struct {
	Name        string            // Required
	Image       string            // Required: OCI reference
	Size        int64             // Base memory in bytes (default: 1GB)
	HotplugSize int64             // Hotplug memory in bytes (default: 3GB)
	Vcpus       int               // Default 2
	Env         map[string]string // Optional environment variables
}

// AttachVolumeRequest is the domain request for attaching a volume
type AttachVolumeRequest struct {
	MountPath string
}
