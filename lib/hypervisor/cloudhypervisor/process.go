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

// ProcessManager implements hypervisor.ProcessManager for Cloud Hypervisor.
type ProcessManager struct{}

// NewProcessManager creates a new Cloud Hypervisor process manager.
func NewProcessManager() *ProcessManager {
	return &ProcessManager{}
}

// Verify ProcessManager implements the interface
var _ hypervisor.ProcessManager = (*ProcessManager)(nil)

// SocketName returns the socket filename for Cloud Hypervisor.
func (p *ProcessManager) SocketName() string {
	return "ch.sock"
}

// StartProcess launches a Cloud Hypervisor VMM process.
func (p *ProcessManager) StartProcess(ctx context.Context, paths *paths.Paths, version string, socketPath string) (int, error) {
	chVersion := vmm.CHVersion(version)
	if !vmm.IsVersionSupported(chVersion) {
		return 0, fmt.Errorf("unsupported cloud-hypervisor version: %s", version)
	}
	return vmm.StartProcess(ctx, paths, chVersion, socketPath)
}

// StartProcessWithArgs launches a Cloud Hypervisor VMM process with extra arguments.
func (p *ProcessManager) StartProcessWithArgs(ctx context.Context, paths *paths.Paths, version string, socketPath string, extraArgs []string) (int, error) {
	chVersion := vmm.CHVersion(version)
	if !vmm.IsVersionSupported(chVersion) {
		return 0, fmt.Errorf("unsupported cloud-hypervisor version: %s", version)
	}
	return vmm.StartProcessWithArgs(ctx, paths, chVersion, socketPath, extraArgs)
}

// GetBinaryPath returns the path to the Cloud Hypervisor binary.
func (p *ProcessManager) GetBinaryPath(paths *paths.Paths, version string) (string, error) {
	chVersion := vmm.CHVersion(version)
	if !vmm.IsVersionSupported(chVersion) {
		return "", fmt.Errorf("unsupported cloud-hypervisor version: %s", version)
	}
	return vmm.GetBinaryPath(paths, chVersion)
}
