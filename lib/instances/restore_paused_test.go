package instances

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/kernel/hypeman/lib/hypervisor"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const pausedRestoreHypervisorType hypervisor.Type = "paused-restore-test"

var pausedRestoreHypervisors sync.Map

func init() {
	hypervisor.RegisterClientFactory(pausedRestoreHypervisorType, func(socketPath string) (hypervisor.Hypervisor, error) {
		hv, ok := pausedRestoreHypervisors.Load(socketPath)
		if !ok {
			return nil, errors.New("missing paused restore hypervisor")
		}
		return hv.(*pausedRestoreHypervisor), nil
	})
}

type pausedRestoreHypervisor struct {
	lifecycleNoopHypervisor
	mu          sync.Mutex
	resumeErr   error
	resumeCalls int
}

func (h *pausedRestoreHypervisor) GetVMInfo(context.Context) (*hypervisor.VMInfo, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return &hypervisor.VMInfo{State: h.state}, nil
}

func (h *pausedRestoreHypervisor) Resume(ctx context.Context) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.resumeCalls++
	if err := ctx.Err(); err != nil {
		return err
	}
	if h.resumeErr != nil {
		return h.resumeErr
	}
	h.state = hypervisor.StateRunning
	return nil
}

func newPausedRestoreManager(t *testing.T, runningState State) (*manager, string, *pausedRestoreHypervisor) {
	t.Helper()
	m, id := newLifecycleNoopManagerWithInstance(t, runningState, time.Now().UTC())
	meta, err := m.loadMetadata(id)
	require.NoError(t, err)
	meta.HypervisorType = pausedRestoreHypervisorType
	require.NoError(t, m.saveMetadata(meta))
	hv := &pausedRestoreHypervisor{lifecycleNoopHypervisor: lifecycleNoopHypervisor{state: hypervisor.StatePaused}}
	pausedRestoreHypervisors.Store(meta.SocketPath, hv)
	t.Cleanup(func() { pausedRestoreHypervisors.Delete(meta.SocketPath) })
	return m, id, hv
}

func TestRestorePausedInstance(t *testing.T) {
	for _, state := range []State{StateRunning, StateInitializing} {
		t.Run(string(state), func(t *testing.T) {
			m, id, hv := newPausedRestoreManager(t, state)
			before, err := os.ReadFile(m.paths.InstanceMetadata(id))
			require.NoError(t, err)
			events, cancel := m.SubscribeLifecycleEvents(LifecycleEventConsumerWaitForState)
			defer cancel()

			inst, err := m.RestoreInstance(t.Context(), id)
			require.NoError(t, err)
			assert.Equal(t, state, inst.State)
			assert.False(t, inst.HasSnapshot)
			assert.Equal(t, 1, hv.resumeCalls)
			after, err := os.ReadFile(m.paths.InstanceMetadata(id))
			require.NoError(t, err)
			assert.Equal(t, before, after)
			require.Len(t, events, 1)
			event := <-events
			assert.Equal(t, LifecycleEventRestore, event.Action)
			assert.Equal(t, state, event.Instance.State)

			_, err = m.RestoreInstance(t.Context(), id)
			require.NoError(t, err)
			assert.Equal(t, 1, hv.resumeCalls)
			assertNoLifecycleEvent(t, events)
		})
	}
}

func TestRestorePausedInstanceRetriesResumeFailure(t *testing.T) {
	m, id, hv := newPausedRestoreManager(t, StateRunning)
	events, cancel := m.SubscribeLifecycleEvents(LifecycleEventConsumerWaitForState)
	defer cancel()
	hv.resumeErr = errors.New("resume failed")

	_, err := m.RestoreInstance(t.Context(), id)
	require.ErrorIs(t, err, hv.resumeErr)
	assert.Equal(t, hypervisor.StatePaused, hv.state)
	assertNoLifecycleEvent(t, events)
	hv.resumeErr = nil

	inst, err := m.RestoreInstance(t.Context(), id)
	require.NoError(t, err)
	assert.Equal(t, StateRunning, inst.State)
	assert.Equal(t, 2, hv.resumeCalls)
}

func TestConcurrentRestorePausedInstanceResumesOnce(t *testing.T) {
	m, id, hv := newPausedRestoreManager(t, StateRunning)
	events, cancel := m.SubscribeLifecycleEvents(LifecycleEventConsumerWaitForState)
	defer cancel()
	start := make(chan struct{})
	results := make(chan error, 2)
	for range 2 {
		go func() {
			<-start
			inst, err := m.RestoreInstance(t.Context(), id)
			if err == nil && inst.State != StateRunning {
				err = errors.New("restored instance is not running")
			}
			results <- err
		}()
	}
	close(start)
	for range 2 {
		assert.NoError(t, <-results)
	}
	assert.Equal(t, 1, hv.resumeCalls)
	require.Len(t, events, 1)
	assert.Equal(t, LifecycleEventRestore, (<-events).Action)
	assertNoLifecycleEvent(t, events)
}
