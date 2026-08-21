package instances

import (
	"time"

	"github.com/kernel/hypeman/lib/autostandby"
	"github.com/kernel/hypeman/lib/healthcheck"
	"github.com/kernel/hypeman/lib/hypervisor"
	"github.com/kernel/hypeman/lib/instances/phasetracking"
	restartpolicy "github.com/kernel/hypeman/lib/restart-policy"
	"github.com/kernel/hypeman/lib/snapshot"
	"github.com/kernel/hypeman/lib/tags"
)

// State represents the instance state
type State string

const (
	StateStopped      State = "Stopped"      // No VMM, no snapshot
	StateCreated      State = "Created"      // VMM created but not booted (CH native)
	StateInitializing State = "Initializing" // VM running, guest init in progress
	StateRunning      State = "Running"      // Guest program started and ready
	StatePaused       State = "Paused"       // VM paused (CH native)
	StateShutdown     State = "Shutdown"     // VM shutdown, VMM exists (CH native)
	StateStandby      State = "Standby"      // No VMM, snapshot exists
	StateUnknown      State = "Unknown"      // Failed to determine state (VMM query failed)
)

type EgressEnforcementMode string

const (
	EgressEnforcementModeAll           EgressEnforcementMode = "all"
	EgressEnforcementModeHTTPHTTPSOnly EgressEnforcementMode = "http_https_only"
)

// VolumeAttachment represents a volume attached to an instance
type VolumeAttachment struct {
	VolumeID    string // Volume ID
	MountPath   string // Mount path in guest
	Readonly    bool   // Whether mounted read-only
	Overlay     bool   // If true, create per-instance overlay for writes (requires Readonly=true)
	OverlaySize int64  // Size of overlay disk in bytes (max diff from base)
}

// NetworkEgressPolicy configures host-mediated outbound networking behavior.
type NetworkEgressPolicy struct {
	Enabled         bool                  // Whether host-mediated egress policy is enabled
	EnforcementMode EgressEnforcementMode // all (default) blocks direct non-proxy TCP egress, http_https_only blocks only 80/443
}

// CredentialSource references where real credential material is loaded from.
type CredentialSource struct {
	Env string // Host env variable name
}

// CredentialInjectAs describes how the credential is materialized for outbound requests.
// Header templating is currently supported; future types (e.g., request signing) can extend this.
type CredentialInjectAs struct {
	Header string // Header name to set/mutate
	Format string // Format template containing ${value}
}

// CredentialInjectRule scopes a credential injection policy to destination hosts.
type CredentialInjectRule struct {
	Hosts []string           // Optional host patterns (api.example.com, *.example.com); empty means all
	As    CredentialInjectAs // Current v1 injection shape
}

// CredentialPolicy configures one host-managed credential brokering policy.
type CredentialPolicy struct {
	Source CredentialSource
	Inject []CredentialInjectRule
}

// StoredMetadata represents instance metadata that is persisted to disk
type StoredMetadata struct {
	// Identification
	Id    string // Auto-generated CUID2
	Name  string
	Image string // OCI reference as supplied by the caller (tag or digest); the display/API value

	// ResolvedImage is the digest-pinned reference (repo@sha256:...) of the
	// image actually resolved at create time. It is the source of truth for
	// locating the rootfs on boot/start/restore, so a mutable tag (last-pull-wins)
	// can't drift the instance to a different image/arch across restarts. Internal;
	// not exposed on the API. Empty for instances created before this field existed
	// (callers fall back to Image via bootImageRef).
	ResolvedImage string

	// Platform is the resolved image platform as os/arch[/variant] (e.g.
	// "linux/amd64"), captured from the pulled image's metadata at create time.
	// Read-only; echoed on the instance API.
	Platform string

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
	Tags           tags.Tags // User-defined key-value tags
	NetworkEnabled bool      // Whether instance has networking enabled (uses default network)
	NetworkEgress  *NetworkEgressPolicy
	Credentials    map[string]CredentialPolicy
	IP             string // Assigned IP address (empty if NetworkEnabled=false)
	MAC            string // Assigned MAC address (empty if NetworkEnabled=false)

	// Attached volumes
	Volumes []VolumeAttachment // Volumes attached to this instance

	// Timestamps (stored for historical tracking)
	CreatedAt time.Time
	ExpiresAt *time.Time // Absolute expiration time; nil disables expiration
	StartedAt *time.Time // Boot epoch start time (set on create/start; preserved across standby restore)
	StoppedAt *time.Time // Last time VM was stopped

	// Boot progress markers (derived from guest serial log sentinels and persisted)
	ProgramStartedAt  *time.Time // Set when guest program handoff/start boundary is reached
	GuestAgentReadyAt *time.Time // Set when guest-agent is ready (unless skip_guest_agent=true)

	// Versions
	KernelVersion string // Kernel version (e.g., "ch-v6.12.9")

	// Hypervisor configuration
	HypervisorType    hypervisor.Type // Hypervisor type (e.g., "cloud-hypervisor")
	HypervisorVersion string          // Hypervisor version (e.g., "v51.1")
	HypervisorPID     *int            // Hypervisor process ID (may be stale after host restart)

	// Firecracker UFFD snapshot restore metadata.
	FirecrackerSnapshotCacheKey     string
	FirecrackerUseUFFDOnNextRestore bool
	FirecrackerUFFDSessionID        string
	FirecrackerUFFDPagerVersion     string

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

	// Command overrides (like docker run <image> <command>)
	Entrypoint []string // Override image entrypoint (nil = use image default)
	Cmd        []string // Override image cmd (nil = use image default)

	// Boot optimizations
	SkipKernelHeaders bool // Skip kernel headers installation (disables DKMS)
	SkipGuestAgent    bool // Skip guest-agent installation (disables exec/stat API)

	// EnableRosetta attaches an Apple Rosetta virtio-fs share so the guest can
	// execute x86-64 binaries. vz on Apple silicon only. Derived internally when
	// the image platform differs from the host; not a user-facing field.
	EnableRosetta bool

	// Snapshot policy defaults for this instance.
	SnapshotPolicy *SnapshotPolicy

	// Pending standby compression plan for the latest standby snapshot.
	// Persisted so server restarts can recover delayed or interrupted jobs.
	PendingStandbyCompression *PendingStandbyCompression

	// Automatic standby policy driven by host-observed inbound TCP activity.
	AutoStandby *autostandby.Policy

	// Workload health check policy. Health is reported separately from lifecycle state.
	HealthCheck *healthcheck.Policy

	// Whole-instance restart supervision policy and runtime status.
	RestartPolicy *restartpolicy.Policy
	RestartStatus restartpolicy.Status

	// Shutdown configuration
	StopTimeout int // Grace period in seconds for graceful stop (0 = use default 5s)

	// Exit information (populated from serial console sentinel when VM stops)
	ExitCode    *int   // App exit code, nil if VM hasn't exited
	ExitMessage string // Human-readable description of exit (e.g., "command not found", "killed by signal 9 (SIGKILL) - OOM")

	// Cumulative time spent in each lifecycle phase. Updated at every state
	// transition by transition orchestration sites (create/start/stop/standby/
	// restore/fork). Consumers use Snapshot() to read live values that include
	// time accrued in the current phase. See lib/instances/phasetracking.
	Phases phasetracking.Tracker `json:"phases,omitempty"`
}

// Instance represents a virtual machine instance with derived runtime state
type Instance struct {
	StoredMetadata

	// Derived fields (not stored in metadata.json)
	State               State   // Derived from socket + VMM query + guest boot markers
	StateError          *string // Error message if state couldn't be determined (non-nil when State=Unknown)
	HasSnapshot         bool    // Derived from filesystem check
	BootMarkersHydrated bool    // True when missing boot markers were hydrated from logs in this read
	HealthCheckRuntime  *healthcheck.Runtime
}

// GetHypervisorType returns the hypervisor type as a string.
// This implements the middleware.HypervisorTyper interface for OTEL enrichment.
func (i *Instance) GetHypervisorType() string {
	return string(i.HypervisorType)
}

// ListInstancesFilter contains optional filters for listing instances.
// All fields are ANDed together: an instance must match every specified filter.
type ListInstancesFilter struct {
	State *State    // Filter by instance state
	Tags  tags.Tags // Filter by tag key-value pairs (all must match)
}

// Matches returns true if the given instance satisfies all filter criteria.
func (f *ListInstancesFilter) Matches(inst *Instance) bool {
	if f == nil {
		return true
	}
	if f.State != nil && inst.State != *f.State {
		return false
	}
	for k, v := range f.Tags {
		if inst.Tags == nil {
			return false
		}
		if actual, ok := inst.Tags[k]; !ok || actual != v {
			return false
		}
	}
	return true
}

// GPUConfig contains GPU configuration for instance creation
type GPUConfig struct {
	Profile string // vGPU profile name (e.g., "L40S-1Q")
}

// CreateInstanceRequest is the domain request for creating an instance
type CreateInstanceRequest struct {
	Name                     string                      // Required
	Image                    string                      // Required: OCI reference
	Platform                 string                      // Optional: target platform as os/arch[/variant]; drives image resolution and auto-pull. Empty means host platform.
	Size                     int64                       // Base memory in bytes (default: 1GB)
	HotplugSize              int64                       // Hotplug memory in bytes (default: 0, set explicitly to enable)
	OverlaySize              int64                       // Overlay disk size in bytes (default: 10GB)
	Vcpus                    int                         // Default 2
	NetworkBandwidthDownload int64                       // Download rate limit bytes/sec (0 = auto, proportional to CPU)
	NetworkBandwidthUpload   int64                       // Upload rate limit bytes/sec (0 = auto, proportional to CPU)
	DiskIOBps                int64                       // Disk I/O rate limit bytes/sec (0 = auto, proportional to CPU)
	Env                      map[string]string           // Optional environment variables
	Tags                     tags.Tags                   // Optional user-defined key-value tags
	NetworkEnabled           bool                        // Whether to enable networking (uses default network)
	NetworkEgress            *NetworkEgressPolicy        // Optional host-mediated egress policy
	Credentials              map[string]CredentialPolicy // Optional host-managed credential brokering policies
	Devices                  []string                    // Device IDs or names to attach (GPU passthrough)
	Volumes                  []VolumeAttachment          // Volumes to attach at creation time
	Hypervisor               hypervisor.Type             // Optional: hypervisor type (defaults to config)
	HypervisorVersion        string                      // Optional: hypervisor version (defaults to configured default)
	GPU                      *GPUConfig                  // Optional: vGPU configuration
	Entrypoint               []string                    // Override image entrypoint (nil = use image default)
	Cmd                      []string                    // Override image cmd (nil = use image default)
	SkipKernelHeaders        bool                        // Skip kernel headers installation (disables DKMS)
	SkipGuestAgent           bool                        // Skip guest-agent installation (disables exec/stat API)
	SnapshotPolicy           *SnapshotPolicy             // Optional snapshot policy defaults for this instance
	AutoStandby              *autostandby.Policy         // Optional automatic standby policy
	HealthCheck              *healthcheck.Policy         // Optional workload health check policy
	RestartPolicy            *restartpolicy.Policy       // Optional whole-instance restart policy
	TTL                      *time.Duration              // Optional lifetime from creation; zero disables expiration
	ExpiresAt                *time.Time                  // Optional absolute expiration time; mutually exclusive with TTL
	AllowSystemVolumeMounts  bool                        // Internal only: permits attaching reserved system volumes. Never populated from API requests.
	SystemVolumeMountPaths   []string                    // Internal only: exact system paths reserved volumes may mount at.
}

// StartInstanceRequest is the domain request for starting a stopped instance
type StartInstanceRequest struct {
	Entrypoint []string // Override entrypoint (nil = keep previous/image default)
	Cmd        []string // Override cmd (nil = keep previous/image default)
}

// UpdateInstanceRequest is the domain request for updating mutable instance properties.
type UpdateInstanceRequest struct {
	Env              map[string]string     // Updated environment variables (merged with existing)
	AutoStandby      *autostandby.Policy   // Replaces the persisted auto-standby policy when non-nil
	HealthCheck      *healthcheck.Policy   // Replaces the persisted health check policy when non-nil
	RestartPolicy    *restartpolicy.Policy // Replaces the persisted restart policy when non-nil
	RestartPolicySet bool                  // True when restart policy was present in the update request
	TTL              *time.Duration        // Relative lifetime from when the update is committed; zero disables expiration
	ExpiresAt        *time.Time            // Absolute expiration time; mutually exclusive with TTL
}

// ForkInstanceRequest is the domain request for forking an instance.
type ForkInstanceRequest struct {
	Name        string // Required: name for the new forked instance
	FromRunning bool   // Optional: allow forking from Running by auto standby/fork/restore
	TargetState State  // Optional: desired final state of forked instance (Stopped, Standby, Running). Empty means inherit source state.
}

// SnapshotKind determines how snapshot data is captured and restored.
type SnapshotKind = snapshot.SnapshotKind

const (
	// SnapshotKindStandby captures snapshot-based standby state (memory/device/disk).
	SnapshotKindStandby = snapshot.SnapshotKindStandby
	// SnapshotKindStopped captures stopped-state disk+metadata only.
	SnapshotKindStopped = snapshot.SnapshotKindStopped
)

// Snapshot is a centrally stored immutable snapshot resource.
type Snapshot = snapshot.Snapshot

// ListSnapshotsFilter contains optional filters for listing snapshots.
type ListSnapshotsFilter = snapshot.ListSnapshotsFilter

// CreateSnapshotRequest is the domain request for creating a snapshot.
type CreateSnapshotRequest struct {
	Kind        SnapshotKind                        // Required: Standby or Stopped
	Name        string                              // Optional: unique per source instance
	Tags        tags.Tags                           // Optional user-defined key-value tags
	Compression *snapshot.SnapshotCompressionConfig // Optional compression override
}

// StandbyInstanceRequest is the domain request for putting an instance into standby.
type StandbyInstanceRequest struct {
	Compression      *snapshot.SnapshotCompressionConfig // Optional compression override
	CompressionDelay *time.Duration                      // Optional standby-only compression delay override
}

// RestoreSnapshotRequest is the domain request for restoring a snapshot in-place.
type RestoreSnapshotRequest struct {
	TargetState      State           // Optional
	TargetHypervisor hypervisor.Type // Optional, allowed only for Stopped snapshots
}

// ForkSnapshotRequest is the domain request for forking from a snapshot.
type ForkSnapshotRequest struct {
	Name             string          // Required: name for the new instance
	TargetState      State           // Optional
	TargetHypervisor hypervisor.Type // Optional, allowed only for Stopped snapshots
}

// SnapshotPolicy defines default snapshot behavior for an instance.
type SnapshotPolicy struct {
	Compression             *snapshot.SnapshotCompressionConfig
	StandbyCompressionDelay *time.Duration
}

// PendingStandbyCompression stores the effective standby compression plan that
// should be recovered after a server restart.
type PendingStandbyCompression struct {
	Policy    snapshot.SnapshotCompressionConfig
	NotBefore time.Time
}

// AttachVolumeRequest is the domain request for attaching a volume (used for API compatibility)
type AttachVolumeRequest struct {
	MountPath string
	Readonly  bool
}
