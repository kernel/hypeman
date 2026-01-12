package instances

import (
	"context"
	"fmt"
	"os"
	"syscall"
	"time"

	"github.com/kernel/hypeman/lib/guest"
	"github.com/kernel/hypeman/lib/hypervisor"
	"github.com/kernel/hypeman/lib/logger"
	"github.com/kernel/hypeman/lib/network"
)

// deleteInstance stops and deletes an instance
func (m *manager) deleteInstance(
	ctx context.Context,
	id string,
) error {
	log := logger.FromContext(ctx)
	log.InfoContext(ctx, "deleting instance", "instance_id", id)

	// 1. Load instance
	meta, err := m.loadMetadata(id)
	if err != nil {
		log.ErrorContext(ctx, "failed to load instance metadata", "instance_id", id, "error", err)
		return err
	}

	inst := m.toInstance(ctx, meta)
	log.DebugContext(ctx, "loaded instance", "instance_id", id, "state", inst.State)

	// 2. Get network allocation BEFORE killing VMM (while we can still query it)
	var networkAlloc *network.Allocation
	if inst.NetworkEnabled {
		log.DebugContext(ctx, "getting network allocation", "instance_id", id)
		networkAlloc, err = m.networkManager.GetAllocation(ctx, id)
		if err != nil {
			log.WarnContext(ctx, "failed to get network allocation, will still attempt cleanup", "instance_id", id, "error", err)
		}
	}

	// 3. Close exec gRPC connection before killing hypervisor to prevent panic
	if dialer, err := hypervisor.NewVsockDialer(inst.HypervisorType, inst.VsockSocket, inst.VsockCID); err == nil {
		guest.CloseConn(dialer.Key())
	}

	// 4. If hypervisor might be running, force kill it
	// Also attempt kill for StateUnknown since we can't be sure if hypervisor is running
	if inst.State.RequiresVMM() || inst.State == StateUnknown {
		log.DebugContext(ctx, "stopping hypervisor", "instance_id", id, "state", inst.State)
		if err := m.killHypervisor(ctx, &inst); err != nil {
			// Log error but continue with cleanup
			// Best effort to clean up even if hypervisor is unresponsive
			log.WarnContext(ctx, "failed to kill hypervisor, continuing with cleanup", "instance_id", id, "error", err)
		}
	}

	// 5. Release network allocation
	if inst.NetworkEnabled {
		log.DebugContext(ctx, "releasing network", "instance_id", id, "network", "default")
		if err := m.networkManager.ReleaseAllocation(ctx, networkAlloc); err != nil {
			// Log error but continue with cleanup
			log.WarnContext(ctx, "failed to release network, continuing with cleanup", "instance_id", id, "error", err)
		}
	}

	// 6. Detach and auto-unbind devices from VFIO
	if len(inst.Devices) > 0 && m.deviceManager != nil {
		for _, deviceID := range inst.Devices {
			log.DebugContext(ctx, "detaching device", "id", id, "device", deviceID)
			// Mark device as detached
			if err := m.deviceManager.MarkDetached(ctx, deviceID); err != nil {
				log.WarnContext(ctx, "failed to mark device as detached", "id", id, "device", deviceID, "error", err)
			}
			// Auto-unbind from VFIO so native driver can reclaim it
			log.InfoContext(ctx, "auto-unbinding device from VFIO", "id", id, "device", deviceID)
			if err := m.deviceManager.UnbindFromVFIO(ctx, deviceID); err != nil {
				// Log but continue - device might already be unbound or in use by another instance
				log.WarnContext(ctx, "failed to unbind device from VFIO", "id", id, "device", deviceID, "error", err)
			}
		}
	}

	// 6b. Detach volumes
	if len(inst.Volumes) > 0 {
		log.DebugContext(ctx, "detaching volumes", "instance_id", id, "count", len(inst.Volumes))
		for _, volAttach := range inst.Volumes {
			if err := m.volumeManager.DetachVolume(ctx, volAttach.VolumeID, id); err != nil {
				// Log error but continue with cleanup
				log.WarnContext(ctx, "failed to detach volume, continuing with cleanup", "instance_id", id, "volume_id", volAttach.VolumeID, "error", err)
			}
		}
	}

	// 7. Delete all instance data
	log.DebugContext(ctx, "deleting instance data", "instance_id", id)
	if err := m.deleteInstanceData(id); err != nil {
		log.ErrorContext(ctx, "failed to delete instance data", "instance_id", id, "error", err)
		return fmt.Errorf("delete instance data: %w", err)
	}

	log.InfoContext(ctx, "instance deleted successfully", "instance_id", id)
	return nil
}

// killHypervisor force kills the hypervisor process without graceful shutdown
// Used only for delete operations where we're removing all data anyway.
// For operations that need graceful shutdown (like standby), use the hypervisor API directly.
func (m *manager) killHypervisor(ctx context.Context, inst *Instance) error {
	log := logger.FromContext(ctx)

	// If we have a PID, kill the process immediately
	if inst.HypervisorPID != nil {
		pid := *inst.HypervisorPID

		// Check if process exists
		if err := syscall.Kill(pid, 0); err == nil {
			// Process exists - kill it immediately with SIGKILL
			// No graceful shutdown needed since we're deleting all data
			log.DebugContext(ctx, "killing hypervisor process", "instance_id", inst.Id, "pid", pid)
			if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
				log.WarnContext(ctx, "failed to kill hypervisor process", "instance_id", inst.Id, "pid", pid, "error", err)
			}

			// Wait for process to die and reap it to prevent zombies
			// SIGKILL should be instant, but give it a moment
			for i := 0; i < 50; i++ { // 50 * 100ms = 5 seconds
				var wstatus syscall.WaitStatus
				wpid, err := syscall.Wait4(pid, &wstatus, syscall.WNOHANG, nil)
				if err != nil || wpid == pid {
					// Process reaped successfully or error (likely ECHILD if already reaped)
					log.DebugContext(ctx, "hypervisor process killed and reaped", "instance_id", inst.Id, "pid", pid)
					break
				}
				if i == 49 {
					log.WarnContext(ctx, "hypervisor process did not exit in time", "instance_id", inst.Id, "pid", pid)
				}
				time.Sleep(100 * time.Millisecond)
			}
		} else {
			log.DebugContext(ctx, "hypervisor process not running", "instance_id", inst.Id, "pid", pid)
		}
	}

	// Clean up socket if it still exists
	os.Remove(inst.SocketPath)

	return nil
}

// WaitForProcessExit polls for a process to exit, returns true if exited within timeout.
// Exported for use in tests.
func WaitForProcessExit(pid int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)

	for time.Now().Before(deadline) {
		// Check if process still exists (signal 0 doesn't kill, just checks existence)
		if err := syscall.Kill(pid, 0); err != nil {
			// Process is gone (ESRCH = no such process)
			return true
		}
		// Still alive, wait a bit before checking again
		// 10ms polling interval balances responsiveness with CPU usage
		time.Sleep(10 * time.Millisecond)
	}

	// Timeout reached, process still exists
	return false
}
