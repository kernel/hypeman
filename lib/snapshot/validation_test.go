package snapshot

import (
	"errors"
	"testing"

	"github.com/kernel/hypeman/lib/hypervisor"
	"github.com/stretchr/testify/require"
)

func TestValidateCreateRequest(t *testing.T) {
	t.Parallel()

	validateName := func(name string) error {
		if name == "bad name" {
			return errors.New("invalid name")
		}
		return nil
	}

	require.NoError(t, ValidateCreateRequest(SnapshotKindStandby, "ok-name", validateName))
	require.NoError(t, ValidateCreateRequest(SnapshotKindStopped, "", validateName))
	require.ErrorIs(t, ValidateCreateRequest("invalid", "ok-name", validateName), ErrInvalidRequest)
	require.ErrorIs(t, ValidateCreateRequest(SnapshotKindStandby, "bad name", validateName), ErrInvalidRequest)
}

func TestValidateForkRequest(t *testing.T) {
	t.Parallel()

	validateName := func(name string) error {
		if name == "" {
			return errors.New("name required")
		}
		return nil
	}

	require.NoError(t, ValidateForkRequest("fork-1", "", validateName))
	require.NoError(t, ValidateForkRequest("fork-1", StateRunning, validateName))
	require.ErrorIs(t, ValidateForkRequest("", StateRunning, validateName), ErrInvalidRequest)
	require.ErrorIs(t, ValidateForkRequest("fork-1", "Invalid", validateName), ErrInvalidRequest)
}

func TestResolveTargetHypervisor(t *testing.T) {
	t.Parallel()

	validate := func(hv hypervisor.Type) error {
		if hv != hypervisor.TypeQEMU {
			return errors.New("unsupported")
		}
		return nil
	}

	hv, err := ResolveTargetHypervisor(SnapshotKindStandby, hypervisor.TypeVZ, "", validate)
	require.NoError(t, err)
	require.Equal(t, hypervisor.TypeVZ, hv)

	hv, err = ResolveTargetHypervisor(SnapshotKindStopped, hypervisor.TypeVZ, hypervisor.TypeQEMU, validate)
	require.NoError(t, err)
	require.Equal(t, hypervisor.TypeQEMU, hv)

	_, err = ResolveTargetHypervisor(SnapshotKindStandby, hypervisor.TypeVZ, hypervisor.TypeQEMU, validate)
	require.ErrorIs(t, err, ErrInvalidRequest)

	_, err = ResolveTargetHypervisor(SnapshotKindStopped, hypervisor.TypeVZ, hypervisor.TypeFirecracker, validate)
	require.ErrorIs(t, err, ErrInvalidRequest)
}
