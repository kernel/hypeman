package instances

import (
	"context"
	"os/exec"
	"strings"

	"github.com/onkernel/hypeman/lib/devices"
	"github.com/onkernel/hypeman/lib/logger"
)

// Ensure instanceLivenessAdapter implements the interface
var _ devices.InstanceLivenessChecker = (*instanceLivenessAdapter)(nil)

// instanceLivenessAdapter adapts instances.Manager to devices.InstanceLivenessChecker
type instanceLivenessAdapter struct {
	manager *manager
}

// NewLivenessChecker creates a new InstanceLivenessChecker that wraps the instances manager.
// This adapter allows the devices package to query instance state without a circular import.
func NewLivenessChecker(m Manager) devices.InstanceLivenessChecker {
	// Type assert to get the concrete manager type
	mgr, ok := m.(*manager)
	if !ok {
		return nil
	}
	return &instanceLivenessAdapter{manager: mgr}
}

// IsInstanceRunning returns true if the instance exists and is in a running state
// (i.e., has an active VMM process). Returns false if the instance doesn't exist
// or is stopped/standby/unknown.
func (a *instanceLivenessAdapter) IsInstanceRunning(ctx context.Context, instanceID string) bool {
	if a.manager == nil {
		return false
	}
	inst, err := a.manager.getInstance(ctx, instanceID)
	if err != nil {
		return false
	}

	// Consider instance "running" if the VMM is active (any of these states means VM is using the device)
	switch inst.State {
	case StateRunning, StatePaused, StateCreated:
		return true
	default:
		// StateStopped, StateStandby, StateShutdown, StateUnknown
		return false
	}
}

// GetInstanceDevices returns the list of device IDs attached to an instance.
// Returns nil if the instance doesn't exist.
func (a *instanceLivenessAdapter) GetInstanceDevices(ctx context.Context, instanceID string) []string {
	if a.manager == nil {
		return nil
	}
	inst, err := a.manager.getInstance(ctx, instanceID)
	if err != nil {
		return nil
	}
	return inst.Devices
}

// ListAllInstanceDevices returns a map of instanceID -> []deviceIDs for all instances.
func (a *instanceLivenessAdapter) ListAllInstanceDevices(ctx context.Context) map[string][]string {
	if a.manager == nil {
		return nil
	}
	instances, err := a.manager.listInstances(ctx)
	if err != nil {
		return nil
	}

	result := make(map[string][]string)
	for _, inst := range instances {
		if len(inst.Devices) > 0 {
			result[inst.Id] = inst.Devices
		}
	}
	return result
}

// DetectSuspiciousVMMProcesses finds cloud-hypervisor processes that don't match
// known instances and logs warnings. Returns the count of suspicious processes found.
// This uses ListInstances (all instances) rather than ListAllInstanceDevices to avoid
// false positives for instances without GPU devices attached.
func (a *instanceLivenessAdapter) DetectSuspiciousVMMProcesses(ctx context.Context) int {
	log := logger.FromContext(ctx)

	if a.manager == nil {
		return 0
	}

	// Find all cloud-hypervisor processes
	cmd := exec.Command("pgrep", "-a", "cloud-hypervisor")
	output, err := cmd.Output()
	if err != nil {
		// pgrep returns exit code 1 if no processes found - that's fine
		return 0
	}

	lines := strings.Split(strings.TrimSpace(string(output)), "\n")
	if len(lines) == 0 || (len(lines) == 1 && lines[0] == "") {
		return 0
	}

	suspiciousCount := 0
	for _, line := range lines {
		if line == "" {
			continue
		}

		// Try to extract socket path from command line to match against known instances
		// cloud-hypervisor command typically includes --api-socket <path>
		socketPath := ""
		parts := strings.Fields(line)
		for i, part := range parts {
			if part == "--api-socket" && i+1 < len(parts) {
				socketPath = parts[i+1]
				break
			}
		}

		// Check if this socket path matches any known instance
		matched := false
		if socketPath != "" {
			// Socket path is typically like /var/lib/hypeman/guests/<id>/ch.sock
			// Try to extract instance ID
			if strings.Contains(socketPath, "/guests/") {
				pathParts := strings.Split(socketPath, "/guests/")
				if len(pathParts) > 1 {
					instancePath := pathParts[1]
					instanceID := strings.Split(instancePath, "/")[0]
					if a.IsInstanceRunning(ctx, instanceID) {
						matched = true
					}
				}
			}
		}

		if !matched {
			log.WarnContext(ctx, "detected untracked cloud-hypervisor process",
				"process_info", line,
				"socket_path", socketPath,
				"remediation", "Run lib/devices/scripts/gpu-reset.sh for manual recovery if needed",
			)
			suspiciousCount++
		}
	}

	return suspiciousCount
}

