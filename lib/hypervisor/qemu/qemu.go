package qemu

import (
	"context"
	"fmt"
	"time"

	"github.com/digitalocean/go-qemu/qemu"
	"github.com/onkernel/hypeman/lib/hypervisor"
)

// QEMU implements hypervisor.Hypervisor for QEMU VMM.
type QEMU struct {
	client     *Client
	socketPath string // for self-removal from pool on error
}

// New returns a QEMU client for the given socket path.
// Uses a connection pool to ensure only one connection per socket exists.
func New(socketPath string) (*QEMU, error) {
	return GetOrCreate(socketPath)
}

// newClient creates a new QEMU client (internal, used by pool).
func newClient(socketPath string) (*QEMU, error) {
	client, err := NewClient(socketPath)
	if err != nil {
		return nil, fmt.Errorf("create qemu client: %w", err)
	}
	return &QEMU{client: client, socketPath: socketPath}, nil
}

// Verify QEMU implements the interface
var _ hypervisor.Hypervisor = (*QEMU)(nil)

// Capabilities returns the features supported by QEMU.
func (q *QEMU) Capabilities() hypervisor.Capabilities {
	return hypervisor.Capabilities{
		SupportsSnapshot:       false, // Not implemented in first pass
		SupportsHotplugMemory:  false, // Not implemented in first pass
		SupportsPause:          true,
		SupportsVsock:          true,
		SupportsGPUPassthrough: true,
	}
}

// DeleteVM removes the VM configuration from QEMU.
// This sends a graceful shutdown signal to the guest.
func (q *QEMU) DeleteVM(ctx context.Context) error {
	if err := q.client.SystemPowerdown(); err != nil {
		Remove(q.socketPath)
		return err
	}
	return nil
}

// Shutdown stops the QEMU process.
func (q *QEMU) Shutdown(ctx context.Context) error {
	if err := q.client.Quit(); err != nil {
		Remove(q.socketPath)
		return err
	}
	// Connection is gone after quit, remove from pool
	Remove(q.socketPath)
	return nil
}

// GetVMInfo returns current VM state.
func (q *QEMU) GetVMInfo(ctx context.Context) (*hypervisor.VMInfo, error) {
	status, err := q.client.Status()
	if err != nil {
		Remove(q.socketPath)
		return nil, fmt.Errorf("query status: %w", err)
	}

	// Map qemu.Status to hypervisor.VMState using typed enum comparison
	var state hypervisor.VMState
	switch status {
	case qemu.StatusRunning:
		state = hypervisor.StateRunning
	case qemu.StatusPaused:
		state = hypervisor.StatePaused
	case qemu.StatusShutdown:
		state = hypervisor.StateShutdown
	case qemu.StatusPreLaunch:
		state = hypervisor.StateCreated
	case qemu.StatusInMigrate, qemu.StatusPostMigrate, qemu.StatusFinishMigrate:
		state = hypervisor.StatePaused
	case qemu.StatusSuspended:
		state = hypervisor.StatePaused
	case qemu.StatusGuestPanicked, qemu.StatusIOError, qemu.StatusInternalError, qemu.StatusWatchdog:
		// Error states - report as running so caller can investigate
		state = hypervisor.StateRunning
	default:
		state = hypervisor.StateRunning
	}

	return &hypervisor.VMInfo{
		State:            state,
		MemoryActualSize: nil, // Not implemented in first pass
	}, nil
}

// Pause suspends VM execution.
func (q *QEMU) Pause(ctx context.Context) error {
	if err := q.client.Stop(); err != nil {
		Remove(q.socketPath)
		return err
	}
	return nil
}

// Resume continues VM execution.
func (q *QEMU) Resume(ctx context.Context) error {
	if err := q.client.Continue(); err != nil {
		Remove(q.socketPath)
		return err
	}
	return nil
}

// Snapshot creates a VM snapshot.
// Not implemented in first pass.
func (q *QEMU) Snapshot(ctx context.Context, destPath string) error {
	return fmt.Errorf("snapshot not supported by QEMU implementation")
}

// ResizeMemory changes the VM's memory allocation.
// Not implemented in first pass.
func (q *QEMU) ResizeMemory(ctx context.Context, bytes int64) error {
	return fmt.Errorf("memory resize not supported by QEMU implementation")
}

// ResizeMemoryAndWait changes the VM's memory allocation and waits for it to stabilize.
// Not implemented in first pass.
func (q *QEMU) ResizeMemoryAndWait(ctx context.Context, bytes int64, timeout time.Duration) error {
	return fmt.Errorf("memory resize not supported by QEMU implementation")
}
