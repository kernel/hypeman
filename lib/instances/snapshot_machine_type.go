package instances

import (
	"fmt"

	"github.com/kernel/hypeman/lib/hypervisor"
	"github.com/kernel/hypeman/lib/hypervisor/qemu"
)

// normalizeSnapshotMachineType derives QEMU's internal board from the target
// backend profile. Switching away from QEMU clears QEMU-only metadata.
func normalizeSnapshotMachineType(machine hypervisor.MachineType, source, target hypervisor.Type) (hypervisor.MachineType, error) {
	switch target {
	case hypervisor.TypeQEMUMicroVM:
		resolved, err := qemu.ResolveMachineType(qemu.MachineTypeMicroVM)
		if err != nil {
			return "", fmt.Errorf("%w: resolve QEMU microvm machine type: %v", ErrInvalidRequest, err)
		}
		return resolved, nil
	case hypervisor.TypeQEMU:
		if source == hypervisor.TypeQEMU {
			return machine, nil
		}
		resolved, err := qemu.ResolveMachineType("")
		if err != nil {
			return "", fmt.Errorf("%w: resolve QEMU machine type: %v", ErrInvalidRequest, err)
		}
		return resolved, nil
	default:
		return "", nil
	}
}
