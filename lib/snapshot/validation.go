package snapshot

import (
	"fmt"

	"github.com/kernel/hypeman/lib/hypervisor"
)

func ValidateCreateRequest(kind SnapshotKind, name string, validateName func(string) error) error {
	if kind != SnapshotKindStandby && kind != SnapshotKindStopped {
		return fmt.Errorf("%w: kind must be one of %s, %s", ErrInvalidRequest, SnapshotKindStandby, SnapshotKindStopped)
	}
	if name != "" {
		if err := validateName(name); err != nil {
			return fmt.Errorf("%w: %v", ErrInvalidRequest, err)
		}
	}
	return nil
}

func ValidateForkRequest(name, targetState string, validateName func(string) error) error {
	if err := validateName(name); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidRequest, err)
	}
	if targetState != "" && targetState != StateStopped && targetState != StateStandby && targetState != StateRunning {
		return fmt.Errorf("%w: invalid target_state %q", ErrInvalidRequest, targetState)
	}
	return nil
}

func ResolveTargetHypervisor(
	kind SnapshotKind,
	sourceHypervisor, requested hypervisor.Type,
	validateHypervisor func(hypervisor.Type) error,
) (hypervisor.Type, error) {
	if requested == "" {
		return sourceHypervisor, nil
	}
	if kind == SnapshotKindStandby {
		return "", fmt.Errorf("%w: target_hypervisor is only allowed for stopped snapshots", ErrInvalidRequest)
	}
	if err := validateHypervisor(requested); err != nil {
		return "", fmt.Errorf("%w: unsupported target hypervisor %q", ErrInvalidRequest, requested)
	}
	return requested, nil
}
