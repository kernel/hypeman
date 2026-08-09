package instances

import (
	"context"
	"fmt"

	"github.com/kernel/hypeman/lib/hypervisor"
	"github.com/kernel/hypeman/lib/logger"
)

func (m *manager) resolveCreateHypervisorVersion(
	ctx context.Context,
	starter hypervisor.VMStarter,
	hvType hypervisor.Type,
	requested string,
) (string, error) {
	if hvType == hypervisor.TypeQEMUMicroVM {
		detected, err := starter.GetVersion(m.paths)
		if err != nil {
			return "", fmt.Errorf("get QEMU version for qemu-microvm: %w", err)
		}
		if requested != "" && requested != detected {
			return "", fmt.Errorf("%w: requested qemu-microvm version %q does not match installed QEMU %q", ErrInvalidRequest, requested, detected)
		}
		return detected, nil
	}

	if requested != "" {
		if _, err := starter.GetBinaryPath(m.paths, requested); err != nil {
			return "", fmt.Errorf("invalid hypervisor version %q: %w", requested, err)
		}
		return requested, nil
	}

	version, err := starter.GetVersion(m.paths)
	if err != nil {
		logger.FromContext(ctx).WarnContext(ctx, "failed to get hypervisor version", "hypervisor", hvType, "error", err)
		return "unknown", nil
	}
	return version, nil
}
