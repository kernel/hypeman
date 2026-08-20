// Package hypervisor provides an abstraction layer for virtual machine managers.
// This allows the instances package to work with different hypervisors
// (e.g., Cloud Hypervisor, QEMU) through a common interface.
package hypervisor

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"net"
	"slices"
	"sync"
	"time"

	"github.com/kernel/hypeman/lib/paths"
)

// Common errors
var (
	// ErrHypervisorNotRunning is returned when trying to connect to a hypervisor
	// that is not currently running or cannot be reconnected to.
	ErrHypervisorNotRunning = errors.New("hypervisor is not running")

	// ErrNotSupported is returned when an operation is not supported by the hypervisor.
	ErrNotSupported = errors.New("operation not supported by this hypervisor")
)

// Type identifies the hypervisor implementation
type Type string

const (
	// TypeCloudHypervisor is the Cloud Hypervisor VMM
	TypeCloudHypervisor Type = "cloud-hypervisor"
	// TypeFirecracker is the Firecracker VMM
	TypeFirecracker Type = "firecracker"
	// TypeQEMU is QEMU with its architecture-native standard board.
	TypeQEMU Type = "qemu"
	// TypeQEMUMicroVM is QEMU with the minimal x86 microvm board.
	TypeQEMUMicroVM Type = "qemu-microvm"
	// TypeVZ is the Virtualization.framework VMM (macOS only)
	TypeVZ Type = "vz"
)

// socketNames maps hypervisor types to their socket filenames.
// Registered by each hypervisor package's init() function.
var socketNames = make(map[Type]string)

// vsockSocketNames maps hypervisor types to their vsock socket filenames.
// Registered by hypervisor packages when they use socket-based vsock routing.
var vsockSocketNames = make(map[Type]string)

// runtimeRegistrations maps hypervisor types to their registrations.
// Registered by each hypervisor package's init() function. Registration is
// platform-gated (see RegisterRuntime), so the map's keys double as the set
// of runtimes supported by this build on the current host platform.
//
// RegisterRuntime is public API, so custom backends may register after init
// — including while capability requests are being served. All access
// therefore goes through runtimeRegistrationsMu; readers snapshot the map
// under the lock and resolve registration callbacks only after releasing it,
// so a slow Capabilities resolver or LaunchCheck never blocks registration
// and callbacks can safely re-enter the registry.
var (
	runtimeRegistrationsMu sync.RWMutex
	runtimeRegistrations   = make(map[Type]RuntimeRegistration)
)

// RegisterSocketName registers the socket filename for a hypervisor type.
// Called by each hypervisor implementation's init() function.
func RegisterSocketName(t Type, name string) {
	socketNames[t] = name
}

// SocketNameForType returns the socket filename for a hypervisor type.
// Falls back to type + ".sock" if not registered.
func SocketNameForType(t Type) string {
	if name, ok := socketNames[t]; ok {
		return name
	}
	return string(t) + ".sock"
}

// RegisterVsockSocketName registers the vsock socket filename for a hypervisor type.
func RegisterVsockSocketName(t Type, name string) {
	vsockSocketNames[t] = name
}

// VsockSocketNameForType returns the vsock socket filename for a hypervisor type.
// Falls back to "vsock.sock" when a hypervisor doesn't require a custom name.
func VsockSocketNameForType(t Type) string {
	if name, ok := vsockSocketNames[t]; ok {
		return name
	}
	return "vsock.sock"
}

// RuntimeRegistration describes how a hypervisor backend reports itself in
// the capability registry.
type RuntimeRegistration struct {
	// Capabilities resolves the runtime's effective capability set. It is
	// evaluated on every registry read — never cached at registration time —
	// so capabilities that depend on configuration applied after package init
	// (for example the configured cloud-hypervisor default version) stay
	// truthful. Required.
	Capabilities func() Capabilities

	// LaunchCheck verifies host launch prerequisites that platform-gated
	// registration cannot express, such as a required system-installed binary
	// (QEMU). nil means registration alone implies launchability (backends
	// that ship an embedded VMM binary). The registry invokes it on every
	// read; a backend may memoize startup readiness when changing host
	// prerequisites requires a process restart.
	LaunchCheck func() error
}

// RegisterRuntime registers a hypervisor backend in the capability registry.
// Backends must register only on platforms where they can genuinely launch
// VMs: the Linux backends (cloud-hypervisor, firecracker, qemu) register from
// linux-only files, and vz registers from its darwin-only package. That keeps
// RegisteredRuntimes truthful without a hand-maintained platform switch.
func RegisterRuntime(t Type, reg RuntimeRegistration) {
	if reg.Capabilities == nil {
		panic(fmt.Sprintf("hypervisor: RegisterRuntime(%s) requires a Capabilities resolver", t))
	}
	runtimeRegistrationsMu.Lock()
	defer runtimeRegistrationsMu.Unlock()
	runtimeRegistrations[t] = reg
}

// RegisterCapabilities registers a static capability set for a hypervisor
// type with no launch check, so registration alone implies launchability.
//
// Deprecated: Use RegisterRuntime, which resolves capabilities per registry
// read (so configuration applied after init stays truthful) and supports an
// optional LaunchCheck for host launch prerequisites. RegisterCapabilities
// is kept so custom backends built against earlier versions of this module
// keep compiling; it wraps RegisterRuntime with a resolver that returns the
// given set unchanged.
func RegisterCapabilities(t Type, caps Capabilities) {
	RegisterRuntime(t, RuntimeRegistration{
		Capabilities: func() Capabilities { return caps },
	})
}

// CapabilitiesForType returns the effective capabilities for a hypervisor
// type, resolved at call time. It reports ok=false for runtimes that cannot
// run on the current host platform, because such runtimes never register.
func CapabilitiesForType(t Type) (Capabilities, bool) {
	runtimeRegistrationsMu.RLock()
	reg, ok := runtimeRegistrations[t]
	runtimeRegistrationsMu.RUnlock()
	if !ok {
		return Capabilities{}, false
	}
	// Resolved after releasing the lock: resolvers may be slow or re-enter
	// the registry.
	return reg.Capabilities(), true
}

// RegisteredRuntime is one capability-registry entry resolved at read time.
type RegisteredRuntime struct {
	Type         Type
	Capabilities Capabilities

	// LaunchErr is non-nil when the runtime is supported by this build on
	// this platform but its launch prerequisites are not currently met (for
	// example QEMU registered on Linux without a system QEMU binary
	// installed). Ordinary launches of the runtime will fail while it is
	// non-nil.
	LaunchErr error
}

// Available reports whether ordinary launches of this runtime can succeed on
// this host: it is registered for the platform and its launch-prerequisite
// check passed.
func (r RegisteredRuntime) Available() bool { return r.LaunchErr == nil }

// RegisteredRuntimes enumerates every registered runtime, sorted by type name
// so output is deterministic. Because registration is platform-gated, the
// result is the set of runtimes the linked backends support on the current
// host; per-entry Available/LaunchErr additionally reflect whether launch
// prerequisites are met right now.
func RegisteredRuntimes() []RegisteredRuntime {
	runtimeRegistrationsMu.RLock()
	snapshot := maps.Clone(runtimeRegistrations)
	runtimeRegistrationsMu.RUnlock()
	// Enumeration resolves capability and launch-check callbacks, so it runs
	// on a snapshot taken under the lock rather than on the live map:
	// concurrent RegisterRuntime calls must never race the iteration, and
	// callbacks must never execute while the registry lock is held.
	return enumerateRuntimes(snapshot)
}

// enumerateRuntimes resolves the map's entries (capabilities and launch
// checks) and sorts them by type name. Split out from RegisteredRuntimes so
// enumeration semantics are testable without mutating the global registry.
func enumerateRuntimes(byType map[Type]RuntimeRegistration) []RegisteredRuntime {
	runtimes := make([]RegisteredRuntime, 0, len(byType))
	for t, reg := range byType {
		rt := RegisteredRuntime{Type: t, Capabilities: reg.Capabilities()}
		if reg.LaunchCheck != nil {
			rt.LaunchErr = reg.LaunchCheck()
		}
		runtimes = append(runtimes, rt)
	}
	slices.SortFunc(runtimes, func(a, b RegisteredRuntime) int {
		return cmp.Compare(a.Type, b.Type)
	})
	return runtimes
}

// VMStarter handles the full VM startup sequence.
// Each hypervisor implements its own startup flow:
// - Cloud Hypervisor: starts process, configures via HTTP API, boots via HTTP API
// - QEMU: converts config to command-line args, starts process (VM runs immediately)
type VMStarter interface {
	// ValidateConfig performs side-effect-free backend validation. Managers call
	// it during preflight; starters must also validate final launch/restore state.
	ValidateConfig(VMConfig) error

	// SocketName returns the socket filename for this hypervisor.
	// Uses short names to stay within Unix socket path length limits (SUN_LEN ~108 bytes).
	SocketName() string

	// GetBinaryPath returns the path to the hypervisor binary, extracting if needed.
	GetBinaryPath(p *paths.Paths, version string) (string, error)

	// GetVersion returns the default or installed hypervisor version.
	GetVersion(p *paths.Paths) (string, error)

	// ResolveVersion validates an optional requested version and returns the
	// concrete binary version to persist for a new VM or snapshot target.
	ResolveVersion(p *paths.Paths, requested string) (string, error)

	// StartVM launches the hypervisor process and boots the VM.
	// Returns the process ID and a Hypervisor client for subsequent operations.
	StartVM(ctx context.Context, p *paths.Paths, version string, socketPath string, config VMConfig) (pid int, hv Hypervisor, err error)

	// RestoreVM starts the hypervisor and restores VM state from a snapshot.
	// Each hypervisor implements its own restore flow:
	// - Cloud Hypervisor: starts process, calls Restore API
	// - QEMU: would start with -incoming or -loadvm flags (not yet implemented)
	// Returns the process ID and a Hypervisor client. The VM is in paused state after restore.
	RestoreVM(ctx context.Context, p *paths.Paths, version string, socketPath string, snapshotPath string, opts RestoreOptions) (pid int, hv Hypervisor, err error)

	// PrepareFork allows hypervisors to prepare forked instance state.
	// For snapshot-based forks, implementations can rewrite snapshot config with
	// fork identity (paths, vsock, network). Hypervisors that don't support fork
	// should return ErrNotSupported.
	PrepareFork(ctx context.Context, req ForkPrepareRequest) (ForkPrepareResult, error)
}

type SnapshotMemoryBackend string

const (
	SnapshotMemoryBackendFile SnapshotMemoryBackend = "file"
	SnapshotMemoryBackendUFFD SnapshotMemoryBackend = "uffd"
)

type RestoreOptions struct {
	SnapshotMemoryBackend     SnapshotMemoryBackend
	SnapshotMemoryBackingPath string
	SnapshotMemoryCacheKey    string
	SnapshotMemorySessionID   string
}

// ForkNetworkConfig contains network identity fields for fork preparation.
type ForkNetworkConfig struct {
	TAPDevice string
	IP        string
	MAC       string
	Netmask   string
}

// ForkPrepareRequest contains hypervisor-specific fork preparation inputs.
type ForkPrepareRequest struct {
	// SnapshotConfigPath is optional. When empty, implementations should only
	// validate fork support and return without snapshot rewrites.
	SnapshotConfigPath string

	SourceDataDir string
	TargetDataDir string

	VsockCID    int64
	VsockSocket string

	SerialLogPath string
	Network       *ForkNetworkConfig
}

// ForkPrepareResult describes which optional fork rewrites were actually applied.
type ForkPrepareResult struct {
	// VsockCIDUpdated indicates whether snapshot state was updated to use
	// ForkPrepareRequest.VsockCID.
	VsockCIDUpdated bool

	// RequiresSnapshotSourceAlias indicates the restored fork still depends on
	// temporarily aliasing the source data directory during snapshot load.
	RequiresSnapshotSourceAlias bool
}

// Hypervisor defines the interface for VM control operations.
// A Hypervisor client is returned by VMStarter.StartVM after the VM is running.
type Hypervisor interface {
	// DeleteVM sends a graceful shutdown signal to the guest.
	DeleteVM(ctx context.Context) error

	// Shutdown stops the VMM process gracefully.
	Shutdown(ctx context.Context) error

	// GetVMInfo returns current VM state information.
	GetVMInfo(ctx context.Context) (*VMInfo, error)

	// Pause suspends VM execution.
	// Check Capabilities().SupportsPause before calling.
	Pause(ctx context.Context) error

	// Resume continues VM execution after pause.
	// Check Capabilities().SupportsPause before calling.
	Resume(ctx context.Context) error

	// Snapshot creates a VM snapshot at the given path.
	// Check Capabilities().SupportsSnapshot before calling.
	Snapshot(ctx context.Context, destPath string) error

	// ResizeMemory changes the VM's memory allocation.
	// Check Capabilities().SupportsHotplugMemory before calling.
	ResizeMemory(ctx context.Context, bytes int64) error

	// ResizeMemoryAndWait changes the VM's memory allocation and waits for it to stabilize.
	// This polls until the actual memory size matches the target or stabilizes.
	// Check Capabilities().SupportsHotplugMemory before calling.
	ResizeMemoryAndWait(ctx context.Context, bytes int64, timeout time.Duration) error

	// SetTargetGuestMemoryBytes adjusts the runtime balloon target so the guest
	// sees the requested amount of RAM.
	// Check Capabilities().SupportsBalloonControl before calling.
	SetTargetGuestMemoryBytes(ctx context.Context, bytes int64) error

	// GetTargetGuestMemoryBytes returns the current guest-visible RAM target after
	// runtime ballooning is applied.
	// Check Capabilities().SupportsBalloonControl before calling.
	GetTargetGuestMemoryBytes(ctx context.Context) (int64, error)

	// Capabilities returns what features this hypervisor supports.
	Capabilities() Capabilities
}

// Capabilities indicates which optional features a hypervisor supports.
// Callers should check these before calling optional methods.
type Capabilities struct {
	// SupportsSnapshot indicates if Snapshot/Restore are available
	SupportsSnapshot bool

	// SupportsFork indicates VMStarter.PrepareFork implements instance
	// forking, which every fork source state needs. It is explicit rather
	// than derived from SupportsSnapshot because the two are independent in
	// both directions: a snapshot-capable backend may still reject
	// PrepareFork with ErrNotSupported, and a backend can fork a stopped
	// source (a disk clone, no machine-state snapshot involved) without
	// snapshot support at all — vz does exactly that on macOS 13. Forking a
	// standby or running source additionally restores/creates snapshots, so
	// those operations require SupportsStandby() as well; SupportsFork alone
	// promises only the stopped-source fork.
	SupportsFork bool

	// SupportsHotplugMemory indicates if ResizeMemory is available
	SupportsHotplugMemory bool

	// SupportsBalloonControl indicates if runtime balloon target changes are available.
	SupportsBalloonControl bool

	// SupportsPause indicates if Pause/Resume are available
	SupportsPause bool

	// SupportsVsock indicates if vsock communication is available
	SupportsVsock bool

	// SupportsUEFIBoot indicates if firmware boot is available.
	SupportsUEFIBoot bool

	// SupportsTPM indicates if a software TPM can be attached at boot.
	SupportsTPM bool

	// SupportsGPUPassthrough indicates if PCI device passthrough is available
	SupportsGPUPassthrough bool

	// SupportsDiskIOLimit indicates if disk I/O rate limiting is available
	SupportsDiskIOLimit bool

	// SupportsGracefulVMMShutdown indicates the hypervisor exposes an API to
	// ask the VMM process itself to exit cleanly.
	SupportsGracefulVMMShutdown bool

	// SupportsSnapshotBaseReuse indicates snapshots can safely reuse a retained
	// on-disk base across restore/standby cycles.
	SupportsSnapshotBaseReuse bool

	// RequiresHostSnapshotVersion means memory snapshots are tied to the
	// currently installed host binary; the backend cannot launch a historical
	// version selected from instance metadata. Generic lifecycle code therefore
	// records the installed version on every cold start and treats a failure to
	// detect it as fatal.
	//
	// For example, qemu-microvm uses the host's qemu-system binary. A standby
	// snapshot written by QEMU 8.2 cannot be restored after the host upgrades to
	// QEMU 9.0. Firecracker snapshots are also version-sensitive, but Firecracker
	// leaves this false because Hypeman can launch the managed binary version
	// recorded in instance metadata; Cloud Hypervisor is managed the same way.
	RequiresHostSnapshotVersion bool

	// SupportsConcurrentForkPrepare indicates stopped/standby forks can prepare
	// separate target snapshots concurrently from the same source.
	SupportsConcurrentForkPrepare bool

	// SupportsDiskResize indicates if live disk resizing (/vm.resize-disk) is available.
	// Cloud Hypervisor v50.0+ only.
	SupportsDiskResize bool

	// UsesDetachableSnapshotMemoryPager indicates restores can be backed by an
	// external snapshot-memory pager that a running VM can later be detached
	// from (populate remaining pages, then release the session).
	UsesDetachableSnapshotMemoryPager bool
}

// VsockDialer provides vsock connectivity to a guest VM.
// Each hypervisor implements its own connection method:
// - Cloud Hypervisor: Unix socket file + text handshake protocol
// - QEMU: Kernel AF_VSOCK with CID-based addressing
type VsockDialer interface {
	// DialVsock connects to the guest on the specified port.
	// Returns a net.Conn that can be used for bidirectional communication.
	DialVsock(ctx context.Context, port int) (net.Conn, error)

	// Key returns a unique identifier for this dialer, used for connection pooling.
	Key() string
}

// VsockDialerFactory creates VsockDialer instances for a hypervisor type.
type VsockDialerFactory func(vsockSocket string, vsockCID int64) VsockDialer

// vsockDialerFactories maps hypervisor types to their dialer factories.
// Registered by each hypervisor package's init() function.
var vsockDialerFactories = make(map[Type]VsockDialerFactory)

// RegisterVsockDialerFactory registers a VsockDialer factory for a hypervisor type.
// Called by each hypervisor implementation's init() function.
func RegisterVsockDialerFactory(t Type, factory VsockDialerFactory) {
	vsockDialerFactories[t] = factory
}

// NewVsockDialer creates a VsockDialer for the given hypervisor type.
// Returns an error if the hypervisor type doesn't have a registered factory.
func NewVsockDialer(hvType Type, vsockSocket string, vsockCID int64) (VsockDialer, error) {
	factory, ok := vsockDialerFactories[hvType]
	if !ok {
		return nil, fmt.Errorf("no vsock dialer registered for hypervisor type: %s", hvType)
	}
	return factory(vsockSocket, vsockCID), nil
}

// ClientFactory creates Hypervisor client instances for a hypervisor type.
type ClientFactory func(socketPath string) (Hypervisor, error)

// clientFactories maps hypervisor types to their client factories.
var clientFactories = make(map[Type]ClientFactory)

// RegisterClientFactory registers a Hypervisor client factory.
func RegisterClientFactory(t Type, factory ClientFactory) {
	clientFactories[t] = factory
}

// NewClient creates a Hypervisor client for the given type and socket.
func NewClient(hvType Type, socketPath string) (Hypervisor, error) {
	factory, ok := clientFactories[hvType]
	if !ok {
		return nil, fmt.Errorf("no client factory registered for hypervisor type: %s", hvType)
	}
	client, err := factory(socketPath)
	if err != nil {
		return nil, err
	}
	return WrapHypervisor(hvType, client), nil
}
