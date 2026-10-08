package instances

import (
	"context"
	"fmt"

	"github.com/kernel/hypeman/lib/hypervisor"
)

// GetVsockDialer returns a VsockDialer for the specified instance.
func (m *manager) GetVsockDialer(ctx context.Context, instanceID string) (hypervisor.VsockDialer, error) {
	inst, err := m.GetInstance(ctx, instanceID)
	if err != nil {
		return nil, err
	}

	if inst.MacOS != nil && !guestAgentEnabled(&inst.StoredMetadata) {
		return nil, fmt.Errorf("%w: macOS image does not enable the shared guest agent", ErrInvalidRequest)
	}
	return hypervisor.NewVsockDialer(hypervisor.Type(inst.HypervisorType), inst.VsockSocket, inst.VsockCID)
}
