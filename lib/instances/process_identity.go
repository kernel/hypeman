package instances

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/kernel/hypeman/lib/devices"
	"github.com/kernel/hypeman/lib/hypervisor"
	"github.com/kernel/hypeman/lib/logger"
)

// linuxBootIDPath is the kernel-provided boot ID used to scope process
// identities to a single host boot.
const linuxBootIDPath = "/proc/sys/kernel/random/boot_id"

// hypervisorSIGKILLWaitTimeout bounds how long stop and delete wait for the
// hypervisor to exit after SIGKILL before reporting it still alive. A process
// that survives SIGKILL is stuck in uninterruptible sleep, and waiting longer
// does not unstick it, so the wait is short to keep stop and delete fast.
const hypervisorSIGKILLWaitTimeout = 2 * time.Second

func (m *manager) vfioTerminationGrace() time.Duration {
	if m.vfioTermGrace > 0 {
		return m.vfioTermGrace
	}
	return hypervisor.VFIOTermGrace
}

// SIGKILL during guest driver init can wedge a VF until the parent GPU is reset.
func (m *manager) terminateThenKill(ctx context.Context, inst *Instance, pid int) error {
	if inst.GPUFramework == devices.VGPUFrameworkVendorVFIO || len(inst.Devices) > 0 {
		if syscall.Kill(pid, syscall.SIGTERM) == nil && WaitForProcessExit(pid, m.vfioTerminationGrace()) {
			return nil
		}
		logger.FromContext(ctx).WarnContext(ctx, "hypervisor with VFIO devices did not exit on SIGTERM; hard-killing, device may wedge if the guest driver was initializing",
			"instance_id", inst.Id, "device_path", inst.GPUDevicePath)
	}
	return killProcessAndWait(pid)
}

// killProcessAndWait SIGKILLs pid and waits for it to exit. A process that
// survives the first wait gets its process group killed too (the hypervisor
// may have spawned children in its own group) and a short grace period. An
// error means the process may still be running, so callers must not tear down
// instance resources.
func killProcessAndWait(pid int) error {
	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
		if err == syscall.ESRCH {
			return nil
		}
		return fmt.Errorf("kill hypervisor process %d: %w", pid, err)
	}
	if WaitForProcessExit(pid, hypervisorSIGKILLWaitTimeout) {
		return nil
	}
	// The process may have spawned children in its own process group.
	_ = syscall.Kill(-pid, syscall.SIGKILL)
	if !WaitForProcessExit(pid, hypervisorSIGKILLWaitTimeout) {
		return fmt.Errorf("hypervisor pid %d did not exit after SIGKILL", pid)
	}
	return nil
}

// HypervisorProcessIdentity identifies a specific hypervisor process across
// PID reuse and host reboots. The zero value means no recorded identity.
// It is embedded anonymously in StoredMetadata so the persisted JSON keys
// (and on-disk metadata format) are unchanged.
type HypervisorProcessIdentity struct {
	HypervisorPID       *int   // Hypervisor process ID (may be stale after host restart)
	HypervisorStartTime uint64 // Start time of HypervisorPID from /proc/<pid>/stat (clock ticks since boot). 0 = unknown.
	HypervisorBootID    string // Linux boot ID recorded with HypervisorStartTime; scopes the process identity across host reboots.
}

// Set records pid's boot-scoped identity as the instance's hypervisor.
func (h *HypervisorProcessIdentity) Set(pid int) {
	h.HypervisorPID = &pid
	h.HypervisorStartTime = processStartTime(pid)
	h.HypervisorBootID = hostBootID()
}

// SetUnconfirmed records a bare PID without the boot-scoped identity token.
// Used when the PID was just proven dead: minting a token would stamp the
// current boot ID (and, if the PID is recycled mid-call, a live start time)
// onto a process that is not the hypervisor. Destructive paths must confirm
// socket ownership before trusting an unconfirmed PID.
func (h *HypervisorProcessIdentity) SetUnconfirmed(pid int) {
	h.HypervisorPID = &pid
	h.HypervisorStartTime = 0
	h.HypervisorBootID = ""
}

// Clear erases the recorded identity. Call whenever the recorded hypervisor
// is known gone or a snapshot/fork must not inherit it.
func (h *HypervisorProcessIdentity) Clear() {
	*h = HypervisorProcessIdentity{}
}

// refreshHypervisorPID refreshes the stored PID for display and other
// non-destructive callers. It trusts a live stored PID without confirming
// socket ownership: hydration runs on every list/get, and its answer never
// authorizes teardown — stop, delete, standby, and the vGPU release guards
// all re-resolve identity through resolveLiveHypervisorPID before acting.
func refreshHypervisorPID(stored *StoredMetadata, state State) {
	if !state.RequiresVMM() && state != StateUnknown {
		return
	}
	if stored.HypervisorPID != nil && ProcessExists(*stored.HypervisorPID) {
		return
	}
	if stored.SocketPath == "" {
		return
	}
	pid, err := hypervisor.ResolveProcessPID(stored.SocketPath)
	if err != nil {
		return
	}
	stored.HypervisorProcessIdentity.Set(pid)
}

// resolveLiveHypervisorPID returns the PID of the live hypervisor that owns
// the instance socket, or 0 when no live hypervisor is found. A live stored PID
// whose recorded boot ID and start time match is returned without socket
// confirmation. It returns an error when socket ownership cannot be confirmed
// because the socket scan itself failed.
func resolveLiveHypervisorPID(id HypervisorProcessIdentity, socketPath string) (int, error) {
	stored := 0
	if id.HypervisorPID != nil {
		if ProcessExists(*id.HypervisorPID) {
			stored = *id.HypervisorPID
		} else {
			// ProcessExists treats zombies as dead, so a direct-child VMM that
			// exited on its own never reaches the Wait4 in WaitForProcessExit
			// and would sit unreaped. Reap it here: WNOHANG leaves a live child
			// untouched, and a recycled or non-child PID fails with ECHILD.
			var status syscall.WaitStatus
			_, _ = syscall.Wait4(*id.HypervisorPID, &status, syscall.WNOHANG, nil)
		}
	}
	if runtime.GOOS != "linux" {
		return stored, nil
	}
	bootID := hostBootID()
	if stored != 0 && id.HypervisorBootID != "" && bootID != "" && id.HypervisorBootID != bootID {
		// The recorded identity is scoped to a previous host boot, so whatever
		// process wears the stored PID now is provably not the recorded
		// hypervisor. Treat the stored PID as dead rather than failing closed.
		stored = 0
	}
	if stored != 0 && id.HypervisorStartTime != 0 && id.HypervisorBootID != "" && bootID != "" && id.HypervisorBootID == bootID {
		if processStartTime(stored) == id.HypervisorStartTime {
			return stored, nil
		}
		stored = 0
	}
	if socketPath == "" {
		if stored != 0 {
			return 0, fmt.Errorf("cannot confirm stored hypervisor PID %d without a socket path", stored)
		}
		return 0, nil
	}
	var resolved int
	var err error
	if stored != 0 && id.HypervisorStartTime == 0 {
		resolved, err = hypervisor.ResolveProcessPIDForOwner(socketPath, stored)
	} else {
		resolved, err = hypervisor.ResolveProcessPID(socketPath)
	}
	return classifyResolvedHypervisorOwner(socketPath, stored, resolved, err)
}

// classifyResolvedHypervisorOwner interprets a socket resolution result for
// resolveLiveHypervisorPID: which live PID, if any, is the recorded
// hypervisor, and whether the ambiguity must fail closed.
func classifyResolvedHypervisorOwner(socketPath string, stored, resolved int, err error) (int, error) {
	switch {
	case err == nil && ProcessExists(resolved):
		return resolved, nil
	case err == nil:
		return 0, nil
	}
	if errors.Is(err, hypervisor.ErrNoOwningProcess) {
		// The socket-owner scan found no process holding the listener. A live
		// hypervisor always holds its control-socket listener, so the recorded
		// hypervisor is gone and a live stored PID is a recycled number — the
		// same conclusion already drawn above when the stored PID is dead.
		// Without this, legacy metadata carrying no boot-scoped identity
		// wedges stop and delete forever once its PID is reused.
		return 0, nil
	}
	if stored != 0 {
		return 0, fmt.Errorf("cannot confirm ownership of socket %s for stored hypervisor PID %d: %w", socketPath, stored, err)
	}
	return 0, fmt.Errorf("cannot confirm ownership of socket %s: %w", socketPath, err)
}

// Ambiguous ownership is treated as live; this must not authorize teardown.
func hypervisorMayBeAlive(id HypervisorProcessIdentity, socketPath string) bool {
	pid, err := resolveLiveHypervisorPID(id, socketPath)
	return err != nil || pid > 0
}

// ProcessExists reports whether pid belongs to a live, non-zombie process.
func ProcessExists(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	if err != nil && err != syscall.EPERM {
		return false
	}
	if runtime.GOOS != "linux" {
		return true
	}
	state, err := readLinuxProcessState(pid)
	if err != nil {
		return true
	}
	return state != "Z"
}

func readLinuxProcessState(pid int) (string, error) {
	statusPath := filepath.Join("/proc", strconv.Itoa(pid), "status")
	data, err := os.ReadFile(statusPath)
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(data), "\n") {
		if !strings.HasPrefix(line, "State:") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			return "", fmt.Errorf("malformed process state in %s", statusPath)
		}
		return fields[1], nil
	}
	return "", fmt.Errorf("process state missing from %s", statusPath)
}

var hostBootID = sync.OnceValue(readHostBootID)

func readHostBootID() string {
	if runtime.GOOS != "linux" {
		return ""
	}
	data, err := os.ReadFile(linuxBootIDPath)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// processStartTime returns the start time (field 22 of /proc/<pid>/stat, clock
// ticks since boot) of pid, or 0 when it cannot be read.
func processStartTime(pid int) uint64 {
	if runtime.GOOS != "linux" || pid <= 0 {
		return 0
	}
	data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if err != nil {
		return 0
	}
	closingParen := strings.LastIndexByte(string(data), ')')
	if closingParen == -1 {
		return 0
	}
	fields := strings.Fields(string(data[closingParen+1:]))
	if len(fields) <= 19 {
		return 0
	}
	startTime, err := strconv.ParseUint(fields[19], 10, 64)
	if err != nil {
		return 0
	}
	return startTime
}
