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
	if requiresHostSnapshotVersion(hvType) {
		detected, err := starter.GetVersion(m.paths)
		if err != nil {
			return "", fmt.Errorf("get version for hypervisor %s: %w", hvType, err)
		}
		if requested != "" && requested != detected {
			return "", fmt.Errorf("%w: requested hypervisor %s version %q does not match installed version %q", ErrInvalidRequest, hvType, requested, detected)
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

func requiresHostSnapshotVersion(hvType hypervisor.Type) bool {
	capabilities, ok := hypervisor.CapabilitiesForType(hvType)
	return ok && capabilities.RequiresHostSnapshotVersion
}
