package builds

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kernel/hypeman/lib/paths"
	"github.com/stretchr/testify/require"
)

func TestMachineOnlyStartRecoversInterruptedBuilds(t *testing.T) {
	for _, status := range []string{StatusQueued, StatusBuilding, StatusPushing} {
		t.Run(status, func(t *testing.T) {
			f := newMachineFixture()
			config := DefaultConfig()
			config.MachineBuildOnly = true
			config.BuilderImage = "linux-builder-must-not-start"
			config.MachineBuild = &MachineBuildBackend{Driver: f, Publisher: &machineJobPublisher{fixture: f}}
			api, err := NewManager(paths.New(t.TempDir()), config, noPriorBuilder{}, nil, nil, machineJobImages{fixture: f}, nil, nil, nil)
			require.NoError(t, err)
			m := api.(*manager)
			policy := &BuildPolicy{}
			require.NoError(t, normalizeMachinePolicy(policy))
			meta := &buildMetadata{ID: "build-test", Status: status, Request: &CreateBuildRequest{MachineBaseImage: "registry/base@" + machineTestDigest, BuildPolicy: policy}}
			require.NoError(t, m.storeMachineSource(context.Background(), meta, []byte("synthetic compressed-source bytes")))
			// A workspace left by the interrupted attempt.
			stale := filepath.Join(m.paths.BuildDir(meta.ID), "machine-build-interrupted")
			require.NoError(t, os.MkdirAll(filepath.Join(stale, "bundle"), 0700))
			require.NoError(t, os.WriteFile(filepath.Join(stale, "bundle", "disk.img"), []byte("disk"), 0600))
			require.NoError(t, writeMetadata(m.paths, &buildMetadata{ID: "linux-job", Status: StatusQueued, Request: &CreateBuildRequest{BuilderID: "linux-builder"}}))
			t.Cleanup(func() {
				require.Eventually(t, func() bool { return m.queue.ActiveCount() == 0 }, 5*time.Second, 10*time.Millisecond)
			})

			// Hold the recovered build active so the second recovery sees it running.
			f.startGate = make(chan struct{})
			require.NoError(t, m.Start(context.Background()))
			require.Eventually(t, func() bool { return m.queue.ActiveCount() == 1 }, 5*time.Second, 10*time.Millisecond)
			m.RecoverPendingBuilds()
			close(f.startGate)
			require.Eventually(t, func() bool { return m.queue.ActiveCount() == 0 && m.queue.PendingCount() == 0 }, 5*time.Second, 10*time.Millisecond)
			build, err := m.GetBuild(context.Background(), meta.ID)
			require.NoError(t, err)
			require.Equal(t, StatusReady, build.Status, "failure: %v", build.Error)
			require.NoDirExists(t, stale, "the interrupted attempt's workspace is removed")
			require.NoFileExists(t, filepath.Join(m.paths.BuildSourceDir(meta.ID), "source.tar.gz"), "source is removed once the image is ready")
			require.False(t, m.ReadyForBuilds(), "Linux builder image must not be prepared")
			var starts int
			for _, call := range f.calls {
				if call == "start" {
					starts++
				}
			}
			require.Equal(t, 1, starts, "recovery must not duplicate an active build")
			linux, err := readMetadata(m.paths, "linux-job")
			require.NoError(t, err)
			require.Equal(t, StatusQueued, linux.Status, "Linux jobs are preserved for Linux-mode recovery")
			require.True(t, m.BuilderHasBuilds("linux-builder"), "unrecovered disk jobs still protect their builders")
		})
	}
}

func TestRecoveryCleansCancelledMachineBuildSource(t *testing.T) {
	f := newMachineFixture()
	config := DefaultConfig()
	config.MachineBuild = &MachineBuildBackend{Driver: f, Publisher: &machineJobPublisher{fixture: f}}
	api, err := NewManager(paths.New(t.TempDir()), config, noPriorBuilder{}, nil, nil, machineJobImages{fixture: f}, nil, nil, nil)
	require.NoError(t, err)
	m := api.(*manager)
	meta := &buildMetadata{
		ID:     "cancelled-build",
		Status: StatusCancelled,
		Request: &CreateBuildRequest{
			MachineBaseImage: "registry/base@" + machineTestDigest,
		},
	}
	require.NoError(t, m.storeMachineSource(context.Background(), meta, []byte("source")))
	meta.Status = StatusCancelled
	require.NoError(t, writeMetadata(m.paths, meta))

	m.RecoverPendingBuilds()

	require.NoFileExists(t, filepath.Join(m.paths.BuildSourceDir(meta.ID), "source.tar.gz"))
}

func TestMachineOnlyStartPreservesBuildsWhenBackendDisabled(t *testing.T) {
	config := DefaultConfig()
	config.MachineBuildOnly = true
	api, err := NewManager(paths.New(t.TempDir()), config, nil, nil, nil, nil, nil, nil, nil)
	require.NoError(t, err)
	m := api.(*manager)
	require.NoError(t, writeMetadata(m.paths, &buildMetadata{ID: "disabled", Status: StatusBuilding, Request: &CreateBuildRequest{MachineBaseImage: "registry/base@" + machineTestDigest}}))
	require.NoError(t, m.Start(context.Background()))
	require.Equal(t, 0, m.queue.ActiveCount())
	meta, err := readMetadata(m.paths, "disabled")
	require.NoError(t, err)
	require.Equal(t, StatusBuilding, meta.Status)
	require.False(t, m.ReadyForBuilds())
}
