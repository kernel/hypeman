//go:build darwin

package instances

import (
	"context"
	"fmt"

	"github.com/kernel/hypeman/lib/hypervisor"
	"github.com/kernel/hypeman/lib/hypervisor/cloudhypervisor"
	"github.com/kernel/hypeman/lib/hypervisor/qemu"
	"github.com/kernel/hypeman/lib/hypervisor/vz"
	"github.com/kernel/hypeman/lib/paths"
	"github.com/kernel/hypeman/lib/uffdpager"
)

func init() {
	platformStarters[hypervisor.TypeCloudHypervisor] = cloudhypervisor.NewStarter()
	platformStarters[hypervisor.TypeQEMU] = qemu.NewStarter()
	platformStarters[hypervisor.TypeVZ] = vz.NewStarter()
}

func configurePlatformStarters(_ context.Context, _ *paths.Paths, _ map[hypervisor.Type]hypervisor.VMStarter, cfg ManagerConfig) (*uffdpager.Supervisor, error) {
	if cfg.FirecrackerSnapshotMemoryBackend == uffdpager.BackendUFFD {
		return nil, fmt.Errorf("firecracker uffd snapshot restore is only supported on linux")
	}
	return nil, nil
}
