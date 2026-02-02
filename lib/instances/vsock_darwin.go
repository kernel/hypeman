//go:build darwin

package instances

import (
	"context"
	"fmt"

	"github.com/kernel/hypeman/lib/hypervisor"
	vzlib "github.com/kernel/hypeman/lib/hypervisor/vz"
)

// GetVsockDialer returns a VsockDialer for the specified instance.
func (m *manager) GetVsockDialer(ctx context.Context, instanceID string) (hypervisor.VsockDialer, error) {
	inst, err := m.GetInstance(ctx, instanceID)
	if err != nil {
		return nil, err
	}

	if inst.HypervisorType == hypervisor.TypeVZ {
		return m.getVZVsockDialer(inst)
	}

	return hypervisor.NewVsockDialer(hypervisor.Type(inst.HypervisorType), inst.VsockSocket, inst.VsockCID)
}

func (m *manager) getVZVsockDialer(inst *Instance) (hypervisor.VsockDialer, error) {
	hvRaw, ok := m.activeHypervisors.Load(inst.Id)
	if !ok {
		return nil, fmt.Errorf("vz VM not active for instance %s", inst.Id)
	}

	hv, ok := hvRaw.(*vzlib.Hypervisor)
	if !ok {
		return nil, fmt.Errorf("unexpected hypervisor type for vz instance: %T", hvRaw)
	}

	return vzlib.VsockDialerWithVM(hv.VM(), inst.VsockSocket), nil
}
