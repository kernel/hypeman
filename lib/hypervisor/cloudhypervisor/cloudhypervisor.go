// Package cloudhypervisor implements the hypervisor.Hypervisor interface
// for Cloud Hypervisor VMM.
package cloudhypervisor

import (
	"context"
	"fmt"
	"time"

	"github.com/onkernel/hypeman/lib/hypervisor"
	"github.com/onkernel/hypeman/lib/vmm"
)

// CloudHypervisor implements hypervisor.Hypervisor for Cloud Hypervisor VMM.
type CloudHypervisor struct {
	client *vmm.VMM
}

// New creates a new Cloud Hypervisor client for an existing VMM socket.
func New(socketPath string) (*CloudHypervisor, error) {
	client, err := vmm.NewVMM(socketPath)
	if err != nil {
		return nil, fmt.Errorf("create vmm client: %w", err)
	}
	return &CloudHypervisor{
		client: client,
	}, nil
}

// Verify CloudHypervisor implements the interface
var _ hypervisor.Hypervisor = (*CloudHypervisor)(nil)

// Capabilities returns the features supported by Cloud Hypervisor.
func (c *CloudHypervisor) Capabilities() hypervisor.Capabilities {
	return hypervisor.Capabilities{
		SupportsSnapshot:       true,
		SupportsHotplugMemory:  true,
		SupportsPause:          true,
		SupportsVsock:          true,
		SupportsGPUPassthrough: true,
	}
}

// DeleteVM removes the VM configuration from Cloud Hypervisor.
func (c *CloudHypervisor) DeleteVM(ctx context.Context) error {
	resp, err := c.client.DeleteVMWithResponse(ctx)
	if err != nil {
		return fmt.Errorf("delete vm: %w", err)
	}
	if resp.StatusCode() != 204 {
		return fmt.Errorf("delete vm failed with status %d: %s", resp.StatusCode(), string(resp.Body))
	}
	return nil
}

// Shutdown stops the VMM process gracefully.
func (c *CloudHypervisor) Shutdown(ctx context.Context) error {
	resp, err := c.client.ShutdownVMMWithResponse(ctx)
	if err != nil {
		return fmt.Errorf("shutdown vmm: %w", err)
	}
	// ShutdownVMM may return various codes, 204 is success
	if resp.StatusCode() != 204 {
		return fmt.Errorf("shutdown vmm failed with status %d", resp.StatusCode())
	}
	return nil
}

// GetVMInfo returns current VM state.
func (c *CloudHypervisor) GetVMInfo(ctx context.Context) (*hypervisor.VMInfo, error) {
	resp, err := c.client.GetVmInfoWithResponse(ctx)
	if err != nil {
		return nil, fmt.Errorf("get vm info: %w", err)
	}
	if resp.StatusCode() != 200 || resp.JSON200 == nil {
		return nil, fmt.Errorf("get vm info failed with status %d", resp.StatusCode())
	}

	// Map Cloud Hypervisor state to hypervisor.VMState
	var state hypervisor.VMState
	switch resp.JSON200.State {
	case vmm.Created:
		state = hypervisor.StateCreated
	case vmm.Running:
		state = hypervisor.StateRunning
	case vmm.Paused:
		state = hypervisor.StatePaused
	case vmm.Shutdown:
		state = hypervisor.StateShutdown
	default:
		return nil, fmt.Errorf("unknown vm state: %s", resp.JSON200.State)
	}

	return &hypervisor.VMInfo{
		State:            state,
		MemoryActualSize: resp.JSON200.MemoryActualSize,
	}, nil
}

// Pause suspends VM execution.
func (c *CloudHypervisor) Pause(ctx context.Context) error {
	resp, err := c.client.PauseVMWithResponse(ctx)
	if err != nil {
		return fmt.Errorf("pause vm: %w", err)
	}
	if resp.StatusCode() != 204 {
		return fmt.Errorf("pause vm failed with status %d", resp.StatusCode())
	}
	return nil
}

// Resume continues VM execution.
func (c *CloudHypervisor) Resume(ctx context.Context) error {
	resp, err := c.client.ResumeVMWithResponse(ctx)
	if err != nil {
		return fmt.Errorf("resume vm: %w", err)
	}
	if resp.StatusCode() != 204 {
		return fmt.Errorf("resume vm failed with status %d", resp.StatusCode())
	}
	return nil
}

// Snapshot creates a VM snapshot.
func (c *CloudHypervisor) Snapshot(ctx context.Context, destPath string) error {
	snapshotURL := "file://" + destPath
	snapshotConfig := vmm.VmSnapshotConfig{DestinationUrl: &snapshotURL}
	resp, err := c.client.PutVmSnapshotWithResponse(ctx, snapshotConfig)
	if err != nil {
		return fmt.Errorf("snapshot: %w", err)
	}
	if resp.StatusCode() != 204 {
		return fmt.Errorf("snapshot failed with status %d", resp.StatusCode())
	}
	return nil
}

// ResizeMemory changes the VM's memory allocation.
func (c *CloudHypervisor) ResizeMemory(ctx context.Context, bytes int64) error {
	resizeConfig := vmm.VmResize{DesiredRam: &bytes}
	resp, err := c.client.PutVmResizeWithResponse(ctx, resizeConfig)
	if err != nil {
		return fmt.Errorf("resize memory: %w", err)
	}
	if resp.StatusCode() != 204 {
		return fmt.Errorf("resize memory failed with status %d", resp.StatusCode())
	}
	return nil
}

// ResizeMemoryAndWait changes the VM's memory allocation and waits for it to stabilize.
// It polls until the actual memory size stabilizes (stops changing) or timeout is reached.
func (c *CloudHypervisor) ResizeMemoryAndWait(ctx context.Context, bytes int64, timeout time.Duration) error {
	// First, request the resize
	if err := c.ResizeMemory(ctx, bytes); err != nil {
		return err
	}

	// Poll until memory stabilizes
	const pollInterval = 20 * time.Millisecond
	deadline := time.Now().Add(timeout)

	var lastSize int64 = -1
	stableCount := 0
	const requiredStableChecks = 3 // Require 3 consecutive stable readings

	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		info, err := c.GetVMInfo(ctx)
		if err != nil {
			return fmt.Errorf("poll memory size: %w", err)
		}

		if info.MemoryActualSize == nil {
			// No memory info available, just return after resize
			return nil
		}

		currentSize := *info.MemoryActualSize

		if currentSize == lastSize {
			stableCount++
			if stableCount >= requiredStableChecks {
				// Memory has stabilized
				return nil
			}
		} else {
			stableCount = 0
			lastSize = currentSize
		}

		time.Sleep(pollInterval)
	}

	// Timeout reached, but resize was requested successfully
	return nil
}
