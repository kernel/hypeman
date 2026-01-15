package instances

import (
	"time"

	"github.com/kernel/hypeman/lib/hypervisor"
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
	StateUnknown  State = "Unknown"  // Failed to determine state (VMM query failed)
)

// VolumeAttachment represents a volume attached to an instance
type VolumeAttachment struct {
	VolumeID    string // Volume ID
	MountPath   string // Mount path in guest
	Readonly    bool   // Whether mounted read-only
	Overlay     bool   // If true, create per-instance overlay for writes (requires Readonly=true)
	OverlaySize int64  // Size of overlay disk in bytes (max diff from base)
}

// StoredMetadata represents instance metadata that is persisted to disk
type StoredMetadata struct {
	// Identification
	Id    string // Auto-generated CUID2
	Name  string
	Image string // OCI reference

	// Resources (matching Cloud Hypervisor terminology)
	Size                     int64 // Base memory in bytes
	HotplugSize              int64 // Hotplug memory in bytes
	OverlaySize              int64 // Overlay disk size in bytes
	Vcpus                    int
	NetworkBandwidthDownload int64 // Download rate limit in bytes/sec (external→VM), 0 = auto
	NetworkBandwidthUpload   int64 // Upload rate limit in bytes/sec (VM→external), 0 = auto
	DiskIOBps                int64 // Disk I/O rate limit in bytes/sec, 0 = auto

	// Configuration
	Env            map[string]string
	NetworkEnabled bool   // Whether instance has networking enabled (uses default network)
	IP             string // Assigned IP address (empty if NetworkEnabled=false)
	MAC            string // Assigned MAC address (empty if NetworkEnabled=false)

	// Attached volumes
	Volumes []VolumeAttachment // Volumes attached to this instance

	// Timestamps (stored for historical tracking)
	CreatedAt time.Time
	StartedAt *time.Time // Last time VM was started
	StoppedAt *time.Time // Last time VM was stopped

	// Versions
	KernelVersion string // Kernel version (e.g., "ch-v6.12.9")

	// Hypervisor configuration
	HypervisorType    hypervisor.Type // Hypervisor type (e.g., "cloud-hypervisor")
	HypervisorVersion string          // Hypervisor version (e.g., "v49.0")
	HypervisorPID     *int            // Hypervisor process ID (may be stale after host restart)

	// Paths
	SocketPath string // Path to API socket
	DataDir    string // Instance data directory

	// vsock configuration
	VsockCID    int64  // Guest vsock Context ID
	VsockSocket string // Host-side vsock socket path

	// Attached devices (GPU passthrough)
	Devices []string // Device IDs attached to this instance

	// GPU configuration (vGPU mode)
	GPUProfile  string // vGPU profile name (e.g., "L40S-1Q")
	GPUMdevUUID string // mdev device UUID
}

// Instance represents a virtual machine instance with derived runtime state
type Instance struct {
	StoredMetadata

	// Derived fields (not stored in metadata.json)
	State       State   // Derived from socket + VMM query
	StateError  *string // Error message if state couldn't be determined (non-nil when State=Unknown)
	HasSnapshot bool    // Derived from filesystem check
}

// GetHypervisorType returns the hypervisor type as a string.
// This implements the middleware.HypervisorTyper interface for OTEL enrichment.
func (i *Instance) GetHypervisorType() string {
	return string(i.HypervisorType)
}

// GPUConfig contains GPU configuration for instance creation
type GPUConfig struct {
	Profile string // vGPU profile name (e.g., "L40S-1Q")
}

// CreateInstanceRequest is the domain request for creating an instance
type CreateInstanceRequest struct {
	Name                     string             // Required
	Image                    string             // Required: OCI reference
	Size                     int64              // Base memory in bytes (default: 1GB)
	HotplugSize              int64              // Hotplug memory in bytes (default: 3GB)
	OverlaySize              int64              // Overlay disk size in bytes (default: 10GB)
	Vcpus                    int                // Default 2
	NetworkBandwidthDownload int64              // Download rate limit bytes/sec (0 = auto, proportional to CPU)
	NetworkBandwidthUpload   int64              // Upload rate limit bytes/sec (0 = auto, proportional to CPU)
	DiskIOBps                int64              // Disk I/O rate limit bytes/sec (0 = auto, proportional to CPU)
	Env                      map[string]string  // Optional environment variables
	NetworkEnabled           bool               // Whether to enable networking (uses default network)
	Devices                  []string           // Device IDs or names to attach (GPU passthrough)
	Volumes                  []VolumeAttachment // Volumes to attach at creation time
	Hypervisor               hypervisor.Type    // Optional: hypervisor type (defaults to config)
	GPU                      *GPUConfig         // Optional: vGPU configuration
}

// AttachVolumeRequest is the domain request for attaching a volume (used for API compatibility)
type AttachVolumeRequest struct {
	MountPath string
	Readonly  bool
}
