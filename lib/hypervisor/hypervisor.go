// Package hypervisor provides an abstraction layer for virtual machine managers.
// This allows the instances package to work with different hypervisors
// (e.g., Cloud Hypervisor, QEMU) through a common interface.
package hypervisor

import (
	"context"
	"fmt"
	"net"
	"time"

	"github.com/onkernel/hypeman/lib/paths"
)

// Type identifies the hypervisor implementation
type Type string

const (
	// TypeCloudHypervisor is the Cloud Hypervisor VMM
	TypeCloudHypervisor Type = "cloud-hypervisor"
	// TypeQEMU is the QEMU VMM
	TypeQEMU Type = "qemu"
)

// socketNames maps hypervisor types to their socket filenames.
// Registered by each hypervisor package's init() function.
var socketNames = make(map[Type]string)

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

// VMStarter handles the full VM startup sequence.
// Each hypervisor implements its own startup flow:
// - Cloud Hypervisor: starts process, configures via HTTP API, boots via HTTP API
// - QEMU: converts config to command-line args, starts process (VM runs immediately)
type VMStarter interface {
	// SocketName returns the socket filename for this hypervisor.
	// Uses short names to stay within Unix socket path length limits (SUN_LEN ~108 bytes).
	SocketName() string

	// GetBinaryPath returns the path to the hypervisor binary, extracting if needed.
	GetBinaryPath(p *paths.Paths, version string) (string, error)

	// GetVersion returns the version of the hypervisor binary.
	// For embedded binaries (Cloud Hypervisor), returns the latest supported version.
	// For system binaries (QEMU), queries the installed binary for its version.
	GetVersion(p *paths.Paths) (string, error)

	// StartVM launches the hypervisor process and boots the VM.
	// Returns the process ID and a Hypervisor client for subsequent operations.
	StartVM(ctx context.Context, p *paths.Paths, version string, socketPath string, config VMConfig) (pid int, hv Hypervisor, err error)

	// RestoreVM starts the hypervisor and restores VM state from a snapshot.
	// Each hypervisor implements its own restore flow:
	// - Cloud Hypervisor: starts process, calls Restore API
	// - QEMU: would start with -incoming or -loadvm flags (not yet implemented)
	// Returns the process ID and a Hypervisor client. The VM is in paused state after restore.
	RestoreVM(ctx context.Context, p *paths.Paths, version string, socketPath string, snapshotPath string) (pid int, hv Hypervisor, err error)
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

	// Capabilities returns what features this hypervisor supports.
	Capabilities() Capabilities
}

// Capabilities indicates which optional features a hypervisor supports.
// Callers should check these before calling optional methods.
type Capabilities struct {
	// SupportsSnapshot indicates if Snapshot/Restore are available
	SupportsSnapshot bool

	// SupportsHotplugMemory indicates if ResizeMemory is available
	SupportsHotplugMemory bool

	// SupportsPause indicates if Pause/Resume are available
	SupportsPause bool

	// SupportsVsock indicates if vsock communication is available
	SupportsVsock bool

	// SupportsGPUPassthrough indicates if PCI device passthrough is available
	SupportsGPUPassthrough bool
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
