package instances

import (
	"context"
	"fmt"
	"os"
	"syscall"
	"time"

	"github.com/onkernel/hypeman/lib/logger"
	"github.com/onkernel/hypeman/lib/network"
)

// deleteInstance stops and deletes an instance
func (m *manager) deleteInstance(
	ctx context.Context,
	id string,
) error {
	log := logger.FromContext(ctx)
	log.InfoContext(ctx, "deleting instance", "id", id)
	
	// 1. Load instance
	meta, err := m.loadMetadata(id)
	if err != nil {
		log.ErrorContext(ctx, "failed to load instance metadata", "id", id, "error", err)
		return err
	}

	inst := m.toInstance(ctx, meta)
	log.DebugContext(ctx, "loaded instance", "id", id, "state", inst.State)

	// 2. Get network allocation BEFORE killing VMM (while we can still query it)
	var networkAlloc *network.Allocation
	if inst.NetworkEnabled {
		log.DebugContext(ctx, "getting network allocation", "id", id)
		networkAlloc, err = m.networkManager.GetAllocation(ctx, id)
		if err != nil {
			log.WarnContext(ctx, "failed to get network allocation, will still attempt cleanup", "id", id, "error", err)
		}
	}

	// 3. If VMM might be running, force kill it
	if inst.State.RequiresVMM() {
		log.DebugContext(ctx, "stopping VMM", "id", id, "state", inst.State)
		if err := m.killVMM(ctx, &inst); err != nil {
			// Log error but continue with cleanup
			// Best effort to clean up even if VMM is unresponsive
			log.WarnContext(ctx, "failed to kill VMM, continuing with cleanup", "id", id, "error", err)
		}
	}

	// 4. Release network allocation
	if inst.NetworkEnabled {
		log.DebugContext(ctx, "releasing network", "id", id, "network", "default")
		if err := m.networkManager.ReleaseNetwork(ctx, networkAlloc); err != nil {
			// Log error but continue with cleanup
			log.WarnContext(ctx, "failed to release network, continuing with cleanup", "id", id, "error", err)
		}
	}

	// 5. Delete all instance data
	log.DebugContext(ctx, "deleting instance data", "id", id)
	if err := m.deleteInstanceData(id); err != nil {
		log.ErrorContext(ctx, "failed to delete instance data", "id", id, "error", err)
		return fmt.Errorf("delete instance data: %w", err)
	}

	log.InfoContext(ctx, "instance deleted successfully", "id", id)
	return nil
}

// killVMM force kills the VMM process without graceful shutdown
// Used only for delete operations where we're removing all data anyway.
// For operations that need graceful shutdown (like standby), use the VMM API directly.
func (m *manager) killVMM(ctx context.Context, inst *Instance) error {
	log := logger.FromContext(ctx)
	
	// If we have a PID, kill the process immediately
	if inst.CHPID != nil {
		pid := *inst.CHPID
		
		// Check if process exists
		if err := syscall.Kill(pid, 0); err == nil {
			// Process exists - kill it immediately with SIGKILL
			// No graceful shutdown needed since we're deleting all data
			log.DebugContext(ctx, "killing VMM process", "id", inst.Id, "pid", pid)
			if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
				log.WarnContext(ctx, "failed to kill VMM process", "id", inst.Id, "pid", pid, "error", err)
			}
			
			// Wait for process to die and reap it to prevent zombies
			// SIGKILL should be instant, but give it a moment
			for i := 0; i < 50; i++ { // 50 * 100ms = 5 seconds
				var wstatus syscall.WaitStatus
				wpid, err := syscall.Wait4(pid, &wstatus, syscall.WNOHANG, nil)
				if err != nil || wpid == pid {
					// Process reaped successfully or error (likely ECHILD if already reaped)
					log.DebugContext(ctx, "VMM process killed and reaped", "id", inst.Id, "pid", pid)
					break
				}
				if i == 49 {
					log.WarnContext(ctx, "VMM process did not exit in time", "id", inst.Id, "pid", pid)
				}
				time.Sleep(100 * time.Millisecond)
			}
		} else {
			log.DebugContext(ctx, "VMM process not running", "id", inst.Id, "pid", pid)
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

