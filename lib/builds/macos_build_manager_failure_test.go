package builds

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/kernel/hypeman/lib/instances"
	"github.com/kernel/hypeman/lib/paths"
	"github.com/stretchr/testify/require"
)

func TestMachineFailureUsesSharedFailedStatusWithoutPublication(t *testing.T) {
	f := newMachineFixture()
	f.fail = "provision"
	config := DefaultConfig()
	config.RegistrySecret = "synthetic-test-secret"
	config.MachineBuild = &MachineBuildBackend{Driver: f, Publisher: f}
	api, err := NewManager(paths.New(t.TempDir()), config, noPriorBuilder{}, nil, nil, machineJobImages{fixture: f}, nil, nil, nil)
	require.NoError(t, err)
	m := api.(*manager)
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
	require.Contains(t, f.calls, "destroy")
	require.Equal(t, "machine build provision failed", *done.Error, "guest error text must remain masked")
}

func TestMachineFailureShowsOnlyRecipeStepAndExitCode(t *testing.T) {
	err := machineFailure("provision", fmt.Errorf("guest said secret-token: %w", &machineStepError{step: 2, code: 7}))
	require.Equal(t, "machine build provision failed: recipe step 2 exited with code 7", err.Error())
	require.Equal(t, "machine build provision failed", machineFailure("provision", fmt.Errorf("secret-token")).Error())
	admission := fmt.Errorf("%w: macOS template identity is already in use", instances.ErrInsufficientResources)
	require.Contains(t, machineFailure("start", admission).Error(), "machine build start failed: insufficient resources")
}
