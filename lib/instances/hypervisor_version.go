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
	version, err := starter.ResolveVersion(m.paths, requested)
	if err == nil {
		return version, nil
	}
	if requested != "" || requiresHostSnapshotVersion(hvType) {
		return "", fmt.Errorf("%w: resolve version for hypervisor %s: %v", ErrInvalidRequest, hvType, err)
	}
	logger.FromContext(ctx).WarnContext(ctx, "failed to resolve hypervisor version", "hypervisor", hvType, "error", err)
	return "unknown", nil
}

func requiresHostSnapshotVersion(hvType hypervisor.Type) bool {
	capabilities, ok := hypervisor.CapabilitiesForType(hvType)
	return ok && capabilities.RequiresHostSnapshotVersion
}
