// Package hypervisor provides an abstraction layer for virtual machine managers.
// This allows the instances package to work with different hypervisors
// (e.g., Cloud Hypervisor, QEMU) through a common interface.
package hypervisor

import (
	"context"
	"time"

	"github.com/onkernel/hypeman/lib/paths"
)

// Type identifies the hypervisor implementation
type Type string

const (
	// TypeCloudHypervisor is the Cloud Hypervisor VMM
	TypeCloudHypervisor Type = "cloud-hypervisor"
	// Future: TypeQEMU Type = "qemu"
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

// Hypervisor defines the interface for VM management operations.
// All hypervisor implementations must implement this interface.
type Hypervisor interface {
	// CreateVM configures the VM with the given configuration.
	// The VM is not started yet after this call.
	CreateVM(ctx context.Context, config VMConfig) error

	// BootVM starts the configured VM.
	// Must be called after CreateVM.
	BootVM(ctx context.Context) error

	// DeleteVM removes the VM configuration.
	// The VMM process may still be running after this call.
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

	// Restore loads a VM from a snapshot at the given path.
	// Check Capabilities().SupportsSnapshot before calling.
	Restore(ctx context.Context, sourcePath string) error

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

// ProcessManager handles hypervisor process lifecycle.
// This is separate from the Hypervisor interface because process management
// happens before/after the VMM socket is available.
type ProcessManager interface {
	// SocketName returns the socket filename for this hypervisor.
	// Uses short names to stay within Unix socket path length limits (SUN_LEN ~108 bytes).
	SocketName() string

	// StartProcess launches the hypervisor process.
	// Returns the process ID of the started hypervisor.
	StartProcess(ctx context.Context, p *paths.Paths, version string, socketPath string) (pid int, err error)

	// StartProcessWithArgs launches the hypervisor process with extra arguments.
	StartProcessWithArgs(ctx context.Context, p *paths.Paths, version string, socketPath string, extraArgs []string) (pid int, err error)

	// GetBinaryPath returns the path to the hypervisor binary, extracting if needed.
	GetBinaryPath(p *paths.Paths, version string) (string, error)
}
