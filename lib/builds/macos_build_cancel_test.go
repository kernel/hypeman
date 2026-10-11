package builds

import (
	"context"
	"testing"
	"time"

	"github.com/kernel/hypeman/lib/paths"
	"github.com/stretchr/testify/require"
)

func TestMachineCancellationInterruptsPhasesWithoutPublishing(t *testing.T) {
	fixture := newMachineFixture()
	fixture.waitForCancel = true
	config := DefaultConfig()
	config.RegistrySecret = "synthetic secret"
	config.MachineBuild = &MachineBuildBackend{Driver: fixture, Publisher: fixture}
	api, e := NewManager(paths.New(t.TempDir()), config, noPriorBuilder{}, nil, nil, machineJobImages{fixture: fixture}, nil, nil, nil)
	require.NoError(t, e)
	build, e := api.CreateBuild(context.Background(), CreateBuildRequest{MachineBaseImage: "registry/base@" + machineTestDigest}, []byte("synthetic source"))
	require.NoError(t, e)
	require.Eventually(t, func() bool {
		meta, e := readMetadata(api.(*manager).paths, build.ID)
		return e == nil && meta.BuilderInstance != nil
	}, 5*time.Second, 10*time.Millisecond)
	require.NoError(t, api.CancelBuild(context.Background(), build.ID))
	require.Eventually(t, func() bool { _, active := api.(*manager).machineBuilds.Load(build.ID); return !active }, 5*time.Second, 10*time.Millisecond)
	done, e := api.GetBuild(context.Background(), build.ID)
	require.NoError(t, e)
	require.Equal(t, StatusCancelled, done.Status)
	require.False(t, fixture.publisherCalled)
	require.Contains(t, fixture.calls, "abort")
	require.Contains(t, fixture.calls, "destroy")
}
