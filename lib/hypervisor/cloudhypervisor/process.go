package cloudhypervisor

import (
	"context"
	"fmt"

	"github.com/onkernel/hypeman/lib/hypervisor"
	"github.com/onkernel/hypeman/lib/paths"
	"github.com/onkernel/hypeman/lib/vmm"
)

func init() {
	hypervisor.RegisterSocketName(hypervisor.TypeCloudHypervisor, "ch.sock")
}

// Starter implements hypervisor.VMStarter for Cloud Hypervisor.
type Starter struct{}

// NewStarter creates a new Cloud Hypervisor starter.
func NewStarter() *Starter {
	return &Starter{}
}

// Verify Starter implements the interface
var _ hypervisor.VMStarter = (*Starter)(nil)

// SocketName returns the socket filename for Cloud Hypervisor.
func (s *Starter) SocketName() string {
	return "ch.sock"
}

// GetBinaryPath returns the path to the Cloud Hypervisor binary.
func (s *Starter) GetBinaryPath(p *paths.Paths, version string) (string, error) {
	chVersion := vmm.CHVersion(version)
	if !vmm.IsVersionSupported(chVersion) {
		return "", fmt.Errorf("unsupported cloud-hypervisor version: %s", version)
	}
	return vmm.GetBinaryPath(p, chVersion)
}

// StartVM launches Cloud Hypervisor, configures the VM, and boots it.
// Returns the process ID and a Hypervisor client for subsequent operations.
func (s *Starter) StartVM(ctx context.Context, p *paths.Paths, version string, socketPath string, config hypervisor.VMConfig) (int, hypervisor.Hypervisor, error) {
	// Validate version
	chVersion := vmm.CHVersion(version)
	if !vmm.IsVersionSupported(chVersion) {
		return 0, nil, fmt.Errorf("unsupported cloud-hypervisor version: %s", version)
	}

	// 1. Start the Cloud Hypervisor process
	pid, err := vmm.StartProcess(ctx, p, chVersion, socketPath)
	if err != nil {
		return 0, nil, fmt.Errorf("start process: %w", err)
	}

	// 2. Create the HTTP client
	hv, err := New(socketPath)
	if err != nil {
		return 0, nil, fmt.Errorf("create client: %w", err)
	}

	// 3. Configure the VM via HTTP API
	vmConfig := ToVMConfig(config)
	resp, err := hv.client.CreateVMWithResponse(ctx, vmConfig)
	if err != nil {
		return 0, nil, fmt.Errorf("create vm: %w", err)
	}
	if resp.StatusCode() != 204 {
		return 0, nil, fmt.Errorf("create vm failed with status %d: %s", resp.StatusCode(), string(resp.Body))
	}

	// 4. Boot the VM via HTTP API
	bootResp, err := hv.client.BootVMWithResponse(ctx)
	if err != nil {
		return 0, nil, fmt.Errorf("boot vm: %w", err)
	}
	if bootResp.StatusCode() != 204 {
		return 0, nil, fmt.Errorf("boot vm failed with status %d: %s", bootResp.StatusCode(), string(bootResp.Body))
	}

	return pid, hv, nil
}

// RestoreVM starts Cloud Hypervisor and restores VM state from a snapshot.
// The VM is in paused state after restore; caller should call Resume() to continue execution.
func (s *Starter) RestoreVM(ctx context.Context, p *paths.Paths, version string, socketPath string, snapshotPath string) (int, hypervisor.Hypervisor, error) {
	// Validate version
	chVersion := vmm.CHVersion(version)
	if !vmm.IsVersionSupported(chVersion) {
		return 0, nil, fmt.Errorf("unsupported cloud-hypervisor version: %s", version)
	}

	// 1. Start the Cloud Hypervisor process
	pid, err := vmm.StartProcess(ctx, p, chVersion, socketPath)
	if err != nil {
		return 0, nil, fmt.Errorf("start process: %w", err)
	}

	// 2. Create the HTTP client
	hv, err := New(socketPath)
	if err != nil {
		return 0, nil, fmt.Errorf("create client: %w", err)
	}

	// 3. Restore from snapshot via HTTP API
	sourceURL := "file://" + snapshotPath
	restoreConfig := vmm.RestoreConfig{
		SourceUrl: sourceURL,
		Prefault:  ptr(false),
	}
	resp, err := hv.client.PutVmRestoreWithResponse(ctx, restoreConfig)
	if err != nil {
		hv.Shutdown(ctx) // Cleanup on failure
		return 0, nil, fmt.Errorf("restore: %w", err)
	}
	if resp.StatusCode() != 204 {
		hv.Shutdown(ctx) // Cleanup on failure
		return 0, nil, fmt.Errorf("restore failed with status %d: %s", resp.StatusCode(), string(resp.Body))
	}

	return pid, hv, nil
}

func ptr[T any](v T) *T {
	return &v
}
