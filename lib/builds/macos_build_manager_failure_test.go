package builds

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/kernel/hypeman/lib/instances"
	"github.com/stretchr/testify/require"
)

func TestMachineFailureUsesSharedFailedStatusWithoutPublication(t *testing.T) {
	for _, tc := range []struct {
		name       string
		phase      string
		resolveErr error
		wantError  string
	}{
		{name: "base resolution", resolveErr: errors.New("sensitive-resolution-error"), wantError: "machine build resolve_base failed"},
		{name: "provision", phase: "provision", wantError: "machine build provision failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newMachineFixture()
			f.fail = tc.phase
			m := newMachineTestManager(t, f, f, machineJobImages{fixture: f, resolveErr: tc.resolveErr}, false)
			build, err := m.CreateBuild(context.Background(), CreateBuildRequest{MachineBaseImage: "registry/base@" + machineTestDigest}, []byte("source"))
			require.NoError(t, err)
			require.Eventually(t, func() bool {
				b, e := m.GetBuild(context.Background(), build.ID)
				return e == nil && b.Status == StatusFailed
			}, 5*time.Second, 10*time.Millisecond)
			done, err := m.GetBuild(context.Background(), build.ID)
			require.NoError(t, err)
			require.Equal(t, StatusFailed, done.Status)
			require.Nil(t, done.ImageDigest)
			require.NotContains(t, f.calls, "publish")
			require.NotContains(t, *done.Error, "sensitive-resolution-error")
			require.Equal(t, tc.wantError, *done.Error)
			if tc.phase == "provision" {
				require.Contains(t, f.calls, "destroy")
			} else {
				require.Empty(t, f.calls)
			}
		})
	}
}

func TestMachineFailureShowsOnlyRecipeStepAndExitCode(t *testing.T) {
	err := machineFailure("provision", fmt.Errorf("guest said secret-token: %w", &machineStepError{step: 2, code: 7}))
	require.Equal(t, "machine build provision failed: recipe step 2 exited with code 7", err.Error())
	require.Equal(t, "machine build provision failed", machineFailure("provision", fmt.Errorf("secret-token")).Error())
	admission := fmt.Errorf("%w: macOS template identity is already in use", instances.ErrInsufficientResources)
	require.Contains(t, machineFailure("start", admission).Error(), "machine build start failed: insufficient resources")
}
