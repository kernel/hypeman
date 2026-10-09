package builds

import (
	"context"
	"testing"
	"time"

	"github.com/kernel/hypeman/lib/paths"
	"github.com/stretchr/testify/require"
)

func TestMachineFailureUsesSharedFailedStatusWithoutPublication(t *testing.T) {
	f := newMachineFixture()
	f.fail = "provision"
	config := DefaultConfig()
	config.RegistrySecret = "synthetic-test-secret"
	config.MachineBuild = &MachineBuildBackend{Driver: f, Publisher: f}
	api, err := NewManager(paths.New(t.TempDir()), config, nil, nil, nil, machineJobImages{fixture: f}, nil, nil, nil)
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
