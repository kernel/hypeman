//go:build darwin

package vz

import (
	"context"
	"fmt"
	"time"

	"github.com/Code-Hex/vz/v3"

	"github.com/kernel/hypeman/lib/hypervisor"
)

// Hypervisor implements hypervisor.Hypervisor for Virtualization.framework.
type Hypervisor struct {
	vm       *vz.VirtualMachine
	vmConfig *vz.VirtualMachineConfiguration
}

// Verify Hypervisor implements the interface
var _ hypervisor.Hypervisor = (*Hypervisor)(nil)

// Capabilities returns the features supported by vz.
func (h *Hypervisor) Capabilities() hypervisor.Capabilities {
	supportsSnapshot := false
	if h.vmConfig != nil {
		valid, err := h.vmConfig.ValidateSaveRestoreSupport()
		supportsSnapshot = err == nil && valid
	}

	return hypervisor.Capabilities{
		SupportsSnapshot:       supportsSnapshot,
		SupportsHotplugMemory:  false,
		SupportsPause:          true,
		SupportsVsock:          true,
		SupportsGPUPassthrough: false,
		SupportsDiskIOLimit:    false,
	}
}

// DeleteVM sends a graceful shutdown signal to the guest.
// This requests the guest to shut down cleanly (like ACPI power button).
func (h *Hypervisor) DeleteVM(ctx context.Context) error {
	if !h.vm.CanRequestStop() {
		return fmt.Errorf("vm cannot accept stop request in current state: %s", h.vm.State())
	}

	success, err := h.vm.RequestStop()
	if err != nil {
		return fmt.Errorf("request stop: %w", err)
	}
	if !success {
		return fmt.Errorf("stop request was not accepted")
	}

	return nil
}

// Shutdown stops the VMM forcefully.
// This is a destructive operation - the guest is stopped without cleanup.
func (h *Hypervisor) Shutdown(ctx context.Context) error {
	if !h.vm.CanStop() {
		// Check if already stopped
		if h.vm.State() == vz.VirtualMachineStateStopped {
			return nil
		}
		return fmt.Errorf("vm cannot be stopped in current state: %s", h.vm.State())
	}

	if err := h.vm.Stop(); err != nil {
		return fmt.Errorf("stop vm: %w", err)
	}

	return nil
}

// GetVMInfo returns current VM state information.
func (h *Hypervisor) GetVMInfo(ctx context.Context) (*hypervisor.VMInfo, error) {
	state := h.vm.State()

	var hvState hypervisor.VMState
	switch state {
	case vz.VirtualMachineStateStopped:
		hvState = hypervisor.StateShutdown
	case vz.VirtualMachineStateRunning:
		hvState = hypervisor.StateRunning
	case vz.VirtualMachineStatePaused:
		hvState = hypervisor.StatePaused
	case vz.VirtualMachineStateStarting:
		hvState = hypervisor.StateCreated
	case vz.VirtualMachineStatePausing, vz.VirtualMachineStateResuming:
		// Transitional states - report as running
		hvState = hypervisor.StateRunning
	case vz.VirtualMachineStateStopping:
		hvState = hypervisor.StateShutdown
	case vz.VirtualMachineStateError:
		hvState = hypervisor.StateShutdown
	default:
		hvState = hypervisor.StateRunning
	}

	return &hypervisor.VMInfo{
		State:            hvState,
		MemoryActualSize: nil, // vz doesn't expose current memory usage
	}, nil
}

// Pause suspends VM execution.
func (h *Hypervisor) Pause(ctx context.Context) error {
	if !h.vm.CanPause() {
		return fmt.Errorf("vm cannot be paused in current state: %s", h.vm.State())
	}

	if err := h.vm.Pause(); err != nil {
		return fmt.Errorf("pause vm: %w", err)
	}

	return nil
}

// Resume continues VM execution after pause.
func (h *Hypervisor) Resume(ctx context.Context) error {
	if !h.vm.CanResume() {
		return fmt.Errorf("vm cannot be resumed in current state: %s", h.vm.State())
	}

	if err := h.vm.Resume(); err != nil {
		return fmt.Errorf("resume vm: %w", err)
	}

	return nil
}

// Snapshot creates a VM snapshot at the given path.
// This is only supported on macOS 14+ on ARM64.
func (h *Hypervisor) Snapshot(ctx context.Context, destPath string) error {
	// Check if snapshot is supported
	valid, err := h.vmConfig.ValidateSaveRestoreSupport()
	if err != nil {
		return fmt.Errorf("snapshot not supported: %w", err)
	}
	if !valid {
		return fmt.Errorf("snapshot not supported for this configuration")
	}

	// VM must be paused before saving state
	if h.vm.State() == vz.VirtualMachineStateRunning {
		if err := h.vm.Pause(); err != nil {
			return fmt.Errorf("pause vm before snapshot: %w", err)
		}
	}

	// Wait for pause to complete
	for h.vm.State() != vz.VirtualMachineStatePaused {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}

	// Save machine state
	if err := h.vm.SaveMachineStateToPath(destPath); err != nil {
		return fmt.Errorf("save machine state: %w", err)
	}

	return nil
}

// ResizeMemory is not supported by vz.
func (h *Hypervisor) ResizeMemory(ctx context.Context, bytes int64) error {
	return fmt.Errorf("memory resize not supported by vz")
}

// ResizeMemoryAndWait is not supported by vz.
func (h *Hypervisor) ResizeMemoryAndWait(ctx context.Context, bytes int64, timeout time.Duration) error {
	return fmt.Errorf("memory resize not supported by vz")
}

// VM returns the underlying vz.VirtualMachine for direct access.
// This is used internally for vsock connections.
func (h *Hypervisor) VM() *vz.VirtualMachine {
	return h.vm
}

// StateChangedNotify returns a channel that receives state changes.
func (h *Hypervisor) StateChangedNotify() <-chan vz.VirtualMachineState {
	return h.vm.StateChangedNotify()
}
