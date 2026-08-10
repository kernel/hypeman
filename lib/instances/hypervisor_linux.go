//go:build linux

package instances

import (
	"context"
	"fmt"

	"github.com/kernel/hypeman/lib/hypervisor"
	"github.com/kernel/hypeman/lib/hypervisor/cloudhypervisor"
	"github.com/kernel/hypeman/lib/hypervisor/firecracker"
	"github.com/kernel/hypeman/lib/hypervisor/qemu"
	"github.com/kernel/hypeman/lib/paths"
	"github.com/kernel/hypeman/lib/uffdpager"
)

func init() {
	platformStarters[hypervisor.TypeCloudHypervisor] = cloudhypervisor.NewStarter()
	platformStarters[hypervisor.TypeFirecracker] = firecracker.NewStarter()
	platformStarters[hypervisor.TypeQEMU] = qemu.NewStarter()
	platformStarters[hypervisor.TypeQEMUMicroVM] = qemu.NewMicroVMStarter()
}

// TODO(uffd-orphan-guard): defensively terminate UFFD guests whose pager is lost.
//
// An orphaned Firecracker guest restored with a UFFD memory backend spins its
// vCPU at 100% forever once its pager has gone away (there is nothing left to
// service its page faults). Today nothing on the manager side reacts to pager
// loss, so a crash/restart of the shared pager can strand running guests.
//
// A safe fix is non-trivial and intentionally deferred here because:
//   - The pager is a single long-lived, version-keyed systemd service shared by
//     ALL UFFD guests in this data dir (see uffdpager.Supervisor); it is not a
//     per-guest process, so "the guest's pager died" means "the shared pager
//     died", which would require fanning out a kill across every running UFFD
//     guest.
//   - The manager does not currently maintain a live pager-PID -> guest-PID map
//     or a pager health watchdog, so adding this correctly means introducing a
//     new monitoring loop and guest-enumeration path.
//   - Done carelessly it could kill healthy guests during a benign pager
//     restart/redeploy, which is a worse failure mode than the spin.
//
// Proposed approach when implemented: have the Supervisor expose a health/watch
// channel; on confirmed pager death (not a transient blip), enumerate instances
// whose StoredMetadata.FirecrackerUFFDSessionID is non-empty and are in a
// running state, and SIGKILL their hypervisor PIDs so they stop spinning rather
// than burning a core indefinitely. The test-side reaper in
// cleanupOrphanedProcesses already mitigates this for CI flakes.
func configurePlatformStarters(ctx context.Context, p *paths.Paths, starters map[hypervisor.Type]hypervisor.VMStarter, cfg ManagerConfig) (*uffdpager.Supervisor, error) {
	if cfg.FirecrackerSnapshotMemoryBackend != uffdpager.BackendUFFD {
		return nil, nil
	}
	pager, err := uffdpager.NewSupervisor(ctx, p.DataDir(), cfg.FirecrackerUFFDCacheMaxBytes)
	if err != nil {
		return nil, fmt.Errorf("start firecracker uffd pager: %w", err)
	}
	starters[hypervisor.TypeFirecracker] = firecracker.NewStarter(firecracker.WithUFFDClient(pager))
	return pager, nil
}
