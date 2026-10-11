package hypervisor

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

type fakeSnapshotSupportValidator struct {
	fakeHypervisor
	err    error
	called bool
}

func (h *fakeSnapshotSupportValidator) ValidateSnapshotSupport(context.Context) error {
	h.called = true
	return h.err
}

func TestTracingPreservesSnapshotSupportValidation(t *testing.T) {
	probeErr := errors.New("configuration probe failed")
	hv := &fakeSnapshotSupportValidator{err: probeErr}
	wrapped := WrapHypervisor(TypeVZ, hv)
	validator, ok := wrapped.(SnapshotSupportValidator)
	require.True(t, ok)
	require.ErrorIs(t, validator.ValidateSnapshotSupport(context.Background()), probeErr)
	require.True(t, hv.called)
	hv.err = nil
	require.NoError(t, validator.ValidateSnapshotSupport(context.Background()))
	unsupported := WrapHypervisor(TypeCloudHypervisor, fakeHypervisor{}).(SnapshotSupportValidator)
	require.ErrorIs(t, unsupported.ValidateSnapshotSupport(context.Background()), ErrNotSupported)
}
