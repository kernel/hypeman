package instances

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/kernel/hypeman/lib/healthcheck"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type healthCheckControllerTestStore struct {
	instances []Instance
	events    chan LifecycleEvent
	runtimes  chan *healthcheck.Runtime
}

func (s *healthCheckControllerTestStore) ListInstances(context.Context, *ListInstancesFilter) ([]Instance, error) {
	return append([]Instance(nil), s.instances...), nil
}

func (s *healthCheckControllerTestStore) SetHealthCheckRuntime(_ context.Context, _ string, runtime *healthcheck.Runtime) error {
	s.runtimes <- healthcheck.CloneRuntime(runtime)
	return nil
}

func (s *healthCheckControllerTestStore) SubscribeLifecycleEvents(LifecycleEventConsumer) (<-chan LifecycleEvent, func()) {
	return s.events, func() {}
}

type healthCheckControllerTestRunner struct {
	result healthcheck.ProbeResult
}

func (r healthCheckControllerTestRunner) Check(context.Context, healthcheck.Instance, *healthcheck.Policy) healthcheck.ProbeResult {
	return r.result
}

type captureHealthCheckControllerTestRunner struct {
	instances chan healthcheck.Instance
	result    healthcheck.ProbeResult
}

func (r captureHealthCheckControllerTestRunner) Check(_ context.Context, inst healthcheck.Instance, _ *healthcheck.Policy) healthcheck.ProbeResult {
	r.instances <- inst
	return r.result
}

type getInstanceHealthCheckControllerTestStore struct {
	*healthCheckControllerTestStore
	instance Instance
}

func (s *getInstanceHealthCheckControllerTestStore) GetInstance(context.Context, string) (*Instance, error) {
	return &s.instance, nil
}

type blockingHealthCheckControllerTestStore struct {
	*healthCheckControllerTestStore
	blockID string
	entered chan struct{}
	unblock chan struct{}
	once    sync.Once
}

func (s *blockingHealthCheckControllerTestStore) SetHealthCheckRuntime(ctx context.Context, id string, runtime *healthcheck.Runtime) error {
	if id == s.blockID && runtime != nil {
		s.once.Do(func() { close(s.entered) })
		select {
		case <-s.unblock:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return s.healthCheckControllerTestStore.SetHealthCheckRuntime(ctx, id, runtime)
}

type blockingHealthCheckControllerTestRunner struct {
	started chan struct{}
	unblock chan struct{}
	result  healthcheck.ProbeResult
}

func (r blockingHealthCheckControllerTestRunner) Check(context.Context, healthcheck.Instance, *healthcheck.Policy) healthcheck.ProbeResult {
	close(r.started)
	<-r.unblock
	return r.result
}

func TestHealthCheckControllerPersistsHealthyStatus(t *testing.T) {
	policy, err := healthcheck.NormalizePolicy(&healthcheck.Policy{
		Type:     healthcheck.TypeExec,
		Interval: "1h",
		Exec:     &healthcheck.ExecCheck{Command: []string{"true"}},
	})
	require.NoError(t, err)

	now := time.Date(2026, 5, 16, 1, 0, 0, 0, time.UTC)
	store := &healthCheckControllerTestStore{
		instances: []Instance{{
			StoredMetadata: StoredMetadata{
				Id:          "inst-1",
				StartedAt:   &now,
				HealthCheck: policy,
			},
			State: StateRunning,
		}},
		events:   make(chan LifecycleEvent),
		runtimes: make(chan *healthcheck.Runtime, 4),
	}
	controller := newHealthCheckController(store, healthCheckControllerOptions{
		Now:    func() time.Time { return now },
		Runner: healthCheckControllerTestRunner{result: healthcheck.ProbeResult{Success: true}},
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- controller.Run(ctx)
	}()

	var healthy *healthcheck.Runtime
	deadline := time.After(time.Second)
	for healthy == nil {
		select {
		case runtime := <-store.runtimes:
			if runtime == nil {
				continue
			}
			if runtime.Status == healthcheck.StatusHealthy {
				healthy = runtime
			}
		case <-deadline:
			t.Fatal("timed out waiting for healthy status")
		}
	}

	assert.Equal(t, 1, healthy.ConsecutiveSuccesses)

	cancel()
	require.NoError(t, <-done)
}

func TestHealthCheckControllerChecksInitializingInstance(t *testing.T) {
	policy, err := healthcheck.NormalizePolicy(&healthcheck.Policy{
		Type:     healthcheck.TypeExec,
		Interval: "1h",
		Exec:     &healthcheck.ExecCheck{Command: []string{"true"}},
	})
	require.NoError(t, err)

	now := time.Date(2026, 5, 16, 1, 0, 0, 0, time.UTC)
	store := &healthCheckControllerTestStore{
		instances: []Instance{{
			StoredMetadata: StoredMetadata{
				Id:          "inst-1",
				StartedAt:   &now,
				HealthCheck: policy,
			},
			State: StateInitializing,
		}},
		events:   make(chan LifecycleEvent),
		runtimes: make(chan *healthcheck.Runtime, 4),
	}
	controller := newHealthCheckController(store, healthCheckControllerOptions{
		Now:    func() time.Time { return now },
		Runner: healthCheckControllerTestRunner{result: healthcheck.ProbeResult{Success: true}},
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- controller.Run(ctx)
	}()

	var healthy *healthcheck.Runtime
	deadline := time.After(time.Second)
	for healthy == nil {
		select {
		case runtime := <-store.runtimes:
			if runtime == nil {
				continue
			}
			if runtime.Status == healthcheck.StatusHealthy {
				healthy = runtime
			}
		case <-deadline:
			t.Fatal("timed out waiting for initializing instance check")
		}
	}

	assert.Equal(t, 1, healthy.ConsecutiveSuccesses)

	cancel()
	require.NoError(t, <-done)
}

func TestHealthCheckControllerResetsRuntimeOnStartEvent(t *testing.T) {
	policy, err := healthcheck.NormalizePolicy(&healthcheck.Policy{
		Type:     healthcheck.TypeExec,
		Interval: "1h",
		Exec:     &healthcheck.ExecCheck{Command: []string{"true"}},
	})
	require.NoError(t, err)

	previousCheckedAt := time.Date(2026, 5, 16, 0, 0, 0, 0, time.UTC)
	startedAt := time.Date(2026, 5, 16, 1, 0, 0, 0, time.UTC)
	store := &healthCheckControllerTestStore{
		events:   make(chan LifecycleEvent),
		runtimes: make(chan *healthcheck.Runtime, 4),
	}
	controller := newHealthCheckController(store, healthCheckControllerOptions{
		Now:    func() time.Time { return startedAt },
		Runner: healthCheckControllerTestRunner{result: healthcheck.ProbeResult{Success: true}},
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- controller.Run(ctx)
	}()

	store.events <- LifecycleEvent{
		Action:     LifecycleEventStart,
		InstanceID: "inst-1",
		Instance: &Instance{
			StoredMetadata: StoredMetadata{
				Id:          "inst-1",
				StartedAt:   &startedAt,
				HealthCheck: policy,
			},
			State: StateRunning,
			HealthCheckRuntime: &healthcheck.Runtime{
				Status:               healthcheck.StatusHealthy,
				StartedAt:            &previousCheckedAt,
				LastCheckedAt:        &previousCheckedAt,
				ConsecutiveSuccesses: 9,
			},
		},
	}

	var healthy *healthcheck.Runtime
	deadline := time.After(time.Second)
	for healthy == nil {
		select {
		case runtime := <-store.runtimes:
			if runtime == nil {
				continue
			}
			if runtime.Status == healthcheck.StatusHealthy {
				healthy = runtime
			}
		case <-deadline:
			t.Fatal("timed out waiting for healthy status")
		}
	}

	require.NotNil(t, healthy.StartedAt)
	assert.Equal(t, startedAt, *healthy.StartedAt)
	assert.Equal(t, 1, healthy.ConsecutiveSuccesses)

	cancel()
	require.NoError(t, <-done)
}

func TestHealthCheckControllerRetriesWhenTimerQueueIsFull(t *testing.T) {
	controller := newHealthCheckController(&healthCheckControllerTestStore{}, healthCheckControllerOptions{
		TimerRetryDelay: 10 * time.Millisecond,
	})
	defer controller.stopAllTimers()

	controller.mu.Lock()
	controller.states["inst-1"] = &healthCheckControllerState{generation: 1}
	controller.mu.Unlock()

	for i := 0; i < cap(controller.timerFired); i++ {
		controller.timerFired <- healthCheckTimerEvent{instanceID: "filled"}
	}

	controller.enqueueTimer("inst-1", 1)

	for i := 0; i < cap(controller.timerFired); i++ {
		<-controller.timerFired
	}

	select {
	case event := <-controller.timerFired:
		assert.Equal(t, "inst-1", event.instanceID)
		assert.Equal(t, uint64(1), event.generation)
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for retried timer event")
	}
}

func TestHealthCheckControllerSkipsStaleProbeAfterRuntimeReset(t *testing.T) {
	policy, err := healthcheck.NormalizePolicy(&healthcheck.Policy{
		Type:     healthcheck.TypeExec,
		Interval: "1h",
		Exec:     &healthcheck.ExecCheck{Command: []string{"true"}},
	})
	require.NoError(t, err)

	startedAt := time.Date(2026, 5, 16, 1, 0, 0, 0, time.UTC)
	previousCheckedAt := startedAt.Add(-time.Minute)
	store := &healthCheckControllerTestStore{
		runtimes: make(chan *healthcheck.Runtime, 2),
	}
	runner := blockingHealthCheckControllerTestRunner{
		started: make(chan struct{}),
		unblock: make(chan struct{}),
		result:  healthcheck.ProbeResult{Success: true},
	}
	controller := newHealthCheckController(store, healthCheckControllerOptions{
		Now:    func() time.Time { return startedAt },
		Runner: runner,
	})

	inst := &Instance{
		StoredMetadata: StoredMetadata{
			Id:          "inst-1",
			StartedAt:   &startedAt,
			HealthCheck: policy,
		},
		State: StateRunning,
		HealthCheckRuntime: &healthcheck.Runtime{
			Status:        healthcheck.StatusHealthy,
			LastCheckedAt: &previousCheckedAt,
		},
	}

	controller.syncInstance(context.Background(), inst, false, false)
	go controller.runCheck(context.Background(), "inst-1", 1)
	<-runner.started

	controller.syncInstance(context.Background(), inst, true, true)
	require.Nil(t, <-store.runtimes)

	close(runner.unblock)

	select {
	case runtime := <-store.runtimes:
		t.Fatalf("stale probe persisted runtime after reset: %#v", runtime)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestHealthCheckControllerRefreshesExecProbeReadiness(t *testing.T) {
	policy, err := healthcheck.NormalizePolicy(&healthcheck.Policy{
		Type:     healthcheck.TypeExec,
		Interval: "1h",
		Exec:     &healthcheck.ExecCheck{Command: []string{"true"}},
	})
	require.NoError(t, err)

	startedAt := time.Date(2026, 5, 16, 1, 0, 0, 0, time.UTC)
	agentReadyAt := startedAt.Add(time.Second)
	store := &getInstanceHealthCheckControllerTestStore{
		healthCheckControllerTestStore: &healthCheckControllerTestStore{
			runtimes: make(chan *healthcheck.Runtime, 1),
		},
		instance: Instance{
			StoredMetadata: StoredMetadata{
				Id:                "inst-1",
				StartedAt:         &startedAt,
				GuestAgentReadyAt: &agentReadyAt,
				HealthCheck:       policy,
			},
			State: StateRunning,
		},
	}
	runner := captureHealthCheckControllerTestRunner{
		instances: make(chan healthcheck.Instance, 1),
		result:    healthcheck.ProbeResult{Success: true},
	}
	controller := newHealthCheckController(store, healthCheckControllerOptions{
		Now:    func() time.Time { return startedAt },
		Runner: runner,
	})

	controller.syncInstance(context.Background(), &Instance{
		StoredMetadata: StoredMetadata{
			Id:          "inst-1",
			StartedAt:   &startedAt,
			HealthCheck: policy,
		},
		State: StateRunning,
	}, false, false)
	controller.runCheck(context.Background(), "inst-1", 1)

	select {
	case inst := <-runner.instances:
		assert.True(t, inst.GuestAgentReady)
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for exec probe")
	}
}

func TestHealthCheckControllerDoesNotHoldControllerLockWhilePersisting(t *testing.T) {
	policy, err := healthcheck.NormalizePolicy(&healthcheck.Policy{
		Type:     healthcheck.TypeExec,
		Interval: "1h",
		Exec:     &healthcheck.ExecCheck{Command: []string{"true"}},
	})
	require.NoError(t, err)

	startedAt := time.Date(2026, 5, 16, 1, 0, 0, 0, time.UTC)
	baseStore := &healthCheckControllerTestStore{
		runtimes: make(chan *healthcheck.Runtime, 4),
	}
	store := &blockingHealthCheckControllerTestStore{
		healthCheckControllerTestStore: baseStore,
		blockID:                        "inst-1",
		entered:                        make(chan struct{}),
		unblock:                        make(chan struct{}),
	}
	controller := newHealthCheckController(store, healthCheckControllerOptions{
		Now:    func() time.Time { return startedAt },
		Runner: healthCheckControllerTestRunner{result: healthcheck.ProbeResult{Success: true}},
	})

	inst1 := &Instance{
		StoredMetadata: StoredMetadata{
			Id:          "inst-1",
			StartedAt:   &startedAt,
			HealthCheck: policy,
		},
		State: StateRunning,
	}
	inst2 := &Instance{
		StoredMetadata: StoredMetadata{
			Id:          "inst-2",
			StartedAt:   &startedAt,
			HealthCheck: policy,
		},
		State: StateRunning,
	}

	controller.syncInstance(context.Background(), inst1, false, false)
	go controller.runCheck(context.Background(), "inst-1", 1)
	<-store.entered

	done := make(chan struct{})
	go func() {
		controller.syncInstance(context.Background(), inst2, true, true)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("syncInstance blocked while another instance persisted health check status")
	}

	close(store.unblock)
}

func TestHealthCheckControllerReleasesPersistenceLocks(t *testing.T) {
	controller := newHealthCheckController(&healthCheckControllerTestStore{}, healthCheckControllerOptions{})

	unlock := controller.lockPersistence("inst-1")

	controller.persistMu.Lock()
	require.Len(t, controller.persistLocks, 1)
	controller.persistMu.Unlock()

	unlock()
	controller.persistMu.Lock()
	require.Empty(t, controller.persistLocks)
	controller.persistMu.Unlock()
}
