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
}

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
