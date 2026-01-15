package qemu

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/digitalocean/go-qemu/qemu"
	"github.com/kernel/hypeman/lib/hypervisor"
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
		SupportsSnapshot:       true,  // Uses QMP migrate file:// for snapshot
		SupportsHotplugMemory:  false, // Not implemented - balloon not configured
		SupportsPause:          true,
		SupportsVsock:          true,
		SupportsGPUPassthrough: true,
		SupportsDiskIOLimit:    true,
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

// Snapshot creates a VM snapshot using QEMU's migrate-to-file mechanism.
// The VM state is saved to destPath/memory file.
// The VM config is copied to destPath for restore (QEMU requires exact arg match).
func (q *QEMU) Snapshot(ctx context.Context, destPath string) error {
	// QEMU uses migrate to file for snapshots
	// The "file:" protocol is deprecated in QEMU 7.2+, use "exec:cat > path" instead
	memoryFile := destPath + "/memory"
	uri := "exec:cat > " + memoryFile
	if err := q.client.Migrate(uri); err != nil {
		Remove(q.socketPath)
		return fmt.Errorf("migrate: %w", err)
	}

	// Wait for migration to complete
	if err := q.client.WaitMigration(ctx, migrationTimeout); err != nil {
		Remove(q.socketPath)
		return fmt.Errorf("wait migration: %w", err)
	}

	// Copy VM config from instance dir to snapshot dir
	// QEMU restore requires exact same command-line args as when snapshot was taken
	instanceDir := filepath.Dir(q.socketPath)
	srcConfig := filepath.Join(instanceDir, vmConfigFile)
	dstConfig := filepath.Join(destPath, vmConfigFile)

	configData, err := os.ReadFile(srcConfig)
	if err != nil {
		return fmt.Errorf("read vm config for snapshot: %w", err)
	}
	if err := os.WriteFile(dstConfig, configData, 0644); err != nil {
		return fmt.Errorf("write vm config to snapshot: %w", err)
	}

	return nil
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
