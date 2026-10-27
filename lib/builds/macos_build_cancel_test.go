package builds

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestMachineCancellationInterruptsPhasesWithoutPublishing(t *testing.T) {
	fixture := newMachineFixture()
	fixture.waitForCancel = true
	m := newMachineTestManager(t, fixture, fixture, nil, false)
	build, e := m.CreateBuild(context.Background(), CreateBuildRequest{MachineBaseImage: "registry/base@" + machineTestDigest}, []byte("synthetic source"))
	require.NoError(t, e)
	require.Eventually(t, func() bool {
		meta, e := readMetadata(m.paths, build.ID)
		return e == nil && meta.BuilderInstance != nil
	}, 5*time.Second, 10*time.Millisecond)
	require.NoError(t, m.CancelBuild(context.Background(), build.ID))
	require.Eventually(t, func() bool { _, active := m.machineBuilds.Load(build.ID); return !active }, 5*time.Second, 10*time.Millisecond)
	done, e := m.GetBuild(context.Background(), build.ID)
	require.NoError(t, e)
	require.Equal(t, StatusCancelled, done.Status)
	require.False(t, fixture.publisherCalled)
	require.Contains(t, fixture.calls, "abort")
	require.Contains(t, fixture.calls, "destroy")
}
