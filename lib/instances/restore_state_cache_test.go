package instances

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/kernel/hypeman/lib/hypervisor"
	"github.com/kernel/hypeman/lib/paths"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type staleCacheRestoreStarter struct {
	manager        *manager
	instanceID     string
	restoreStarted chan struct{}
	finishRestore  chan struct{}
	restoreCalls   int
}

func (s *staleCacheRestoreStarter) ValidateConfig(hypervisor.VMConfig) error { return nil }
func (s *staleCacheRestoreStarter) SocketName() string                       { return "stale-cache-test.sock" }
func (s *staleCacheRestoreStarter) GetBinaryPath(*paths.Paths, string) (string, error) {
	return "", nil
}
func (s *staleCacheRestoreStarter) GetVersion(*paths.Paths) (string, error) { return "test", nil }
func (s *staleCacheRestoreStarter) ResolveVersion(*paths.Paths, string) (string, error) {
	return "test", nil
}
func (s *staleCacheRestoreStarter) StartVM(context.Context, *paths.Paths, string, string, hypervisor.VMConfig) (int, hypervisor.Hypervisor, error) {
	return 0, nil, nil
}
func (s *staleCacheRestoreStarter) RestoreVM(_ context.Context, _ *paths.Paths, _ string, socketPath string, _ string, _ hypervisor.RestoreOptions) (int, hypervisor.Hypervisor, error) {
	s.restoreCalls++
	if err := os.MkdirAll(filepath.Dir(socketPath), 0o755); err != nil {
		return 0, nil, err
	}
	if err := os.WriteFile(socketPath, []byte("socket"), 0o644); err != nil {
		return 0, nil, err
	}

	// A concurrent state query can observe the VMM before Resume completes.
	s.manager.storeCachedHypervisorState(s.instanceID, hypervisor.StateCreated)
	close(s.restoreStarted)
	<-s.finishRestore
	return 1, lifecycleNoopHypervisor{state: hypervisor.StateRunning}, nil
}
func (s *staleCacheRestoreStarter) PrepareFork(context.Context, hypervisor.ForkPrepareRequest) (hypervisor.ForkPrepareResult, error) {
	return hypervisor.ForkPrepareResult{}, nil
}

func TestConcurrentRestoreClearsTransitionalHypervisorStateCache(t *testing.T) {
	manager, _ := setupTestManager(t)
	instanceID := "restore-stale-state-cache"
	hvType := hypervisor.Type("restore-stale-state-cache-test")
	starter := &staleCacheRestoreStarter{
		manager:        manager,
		instanceID:     instanceID,
		restoreStarted: make(chan struct{}),
		finishRestore:  make(chan struct{}),
	}
	manager.vmStarters[hvType] = starter
	createStandbySnapshotSourceFixture(t, manager, instanceID, instanceID, hvType)

	type restoreResult struct {
		instance *Instance
		err      error
	}
	firstResult := make(chan restoreResult, 1)
	go func() {
		instance, err := manager.RestoreInstance(context.Background(), instanceID)
		firstResult <- restoreResult{instance: instance, err: err}
	}()
	<-starter.restoreStarted

	secondStarted := make(chan struct{})
	secondResult := make(chan restoreResult, 1)
	go func() {
		close(secondStarted)
		instance, err := manager.RestoreInstance(context.Background(), instanceID)
		secondResult <- restoreResult{instance: instance, err: err}
	}()
	<-secondStarted
	close(starter.finishRestore)

	first := <-firstResult
	require.NoError(t, first.err)
	assert.Equal(t, StateInitializing, first.instance.State)

	second := <-secondResult
	require.NoError(t, second.err)
	assert.Equal(t, StateInitializing, second.instance.State)
	assert.Equal(t, 1, starter.restoreCalls)
}
