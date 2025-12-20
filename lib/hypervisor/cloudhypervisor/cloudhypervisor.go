// Package cloudhypervisor implements the hypervisor.Hypervisor interface
// for Cloud Hypervisor VMM.
package cloudhypervisor

import (
	"context"
	"fmt"

	"github.com/onkernel/hypeman/lib/hypervisor"
	"github.com/onkernel/hypeman/lib/vmm"
)

// CloudHypervisor implements hypervisor.Hypervisor for Cloud Hypervisor VMM.
type CloudHypervisor struct {
	client     *vmm.VMM
	socketPath string
}

// New creates a new Cloud Hypervisor client for an existing VMM socket.
func New(socketPath string) (*CloudHypervisor, error) {
	client, err := vmm.NewVMM(socketPath)
	if err != nil {
		return nil, fmt.Errorf("create vmm client: %w", err)
	}
	return &CloudHypervisor{
		client:     client,
		socketPath: socketPath,
	}, nil
}

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

// CreateVM configures the VM in Cloud Hypervisor.
func (c *CloudHypervisor) CreateVM(ctx context.Context, config hypervisor.VMConfig) error {
	vmConfig := ToVMConfig(config)
	resp, err := c.client.CreateVMWithResponse(ctx, vmConfig)
	if err != nil {
		return fmt.Errorf("create vm: %w", err)
	}
	if resp.StatusCode() != 204 {
		return fmt.Errorf("create vm failed with status %d: %s", resp.StatusCode(), string(resp.Body))
	}
	return nil
}

// BootVM starts the configured VM.
func (c *CloudHypervisor) BootVM(ctx context.Context) error {
	resp, err := c.client.BootVMWithResponse(ctx)
	if err != nil {
		return fmt.Errorf("boot vm: %w", err)
	}
	if resp.StatusCode() != 204 {
		return fmt.Errorf("boot vm failed with status %d: %s", resp.StatusCode(), string(resp.Body))
	}
	return nil
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

	return &hypervisor.VMInfo{State: state}, nil
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

// Restore loads a VM from snapshot.
func (c *CloudHypervisor) Restore(ctx context.Context, sourcePath string) error {
	sourceURL := "file://" + sourcePath
	restoreConfig := vmm.RestoreConfig{
		SourceUrl: sourceURL,
		Prefault:  ptr(false),
	}
	resp, err := c.client.PutVmRestoreWithResponse(ctx, restoreConfig)
	if err != nil {
		return fmt.Errorf("restore: %w", err)
	}
	if resp.StatusCode() != 204 {
		return fmt.Errorf("restore failed with status %d", resp.StatusCode())
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

func ptr[T any](v T) *T {
	return &v
}
