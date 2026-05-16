package healthcheck

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type controllerTestStore struct {
	instances []Instance
	events    chan InstanceEvent
	runtimes  chan *Runtime
}

func (s *controllerTestStore) ListInstances(context.Context) ([]Instance, error) {
	return append([]Instance(nil), s.instances...), nil
}

func (s *controllerTestStore) SetRuntime(_ context.Context, _ string, runtime *Runtime) error {
	s.runtimes <- CloneRuntime(runtime)
	return nil
}

func (s *controllerTestStore) SubscribeInstanceEvents() (<-chan InstanceEvent, func(), error) {
	return s.events, func() {}, nil
}

type controllerTestRunner struct {
	result ProbeResult
}

func (r controllerTestRunner) Check(context.Context, Instance, *Policy) ProbeResult {
	return r.result
}

func TestControllerPersistsHealthyStatus(t *testing.T) {
	policy, err := NormalizePolicy(&Policy{
		Type:     TypeExec,
		Interval: "1h",
		Exec:     &ExecCheck{Command: []string{"true"}},
	})
	require.NoError(t, err)

	now := time.Date(2026, 5, 16, 1, 0, 0, 0, time.UTC)
	store := &controllerTestStore{
		instances: []Instance{{
			ID:              "inst-1",
			State:           StateRunning,
			GuestAgentReady: true,
			HealthCheck:     policy,
		}},
		events:   make(chan InstanceEvent),
		runtimes: make(chan *Runtime, 4),
	}
	controller := NewController(store, controllerTestRunner{result: ProbeResult{Success: true}}, ControllerOptions{
		Now: func() time.Time { return now },
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- controller.Run(ctx)
	}()

	var healthy *Runtime
	deadline := time.After(time.Second)
	for healthy == nil {
		select {
		case runtime := <-store.runtimes:
			if runtime.Status == StatusHealthy {
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

func TestControllerResetsRuntimeOnStartEvent(t *testing.T) {
	policy, err := NormalizePolicy(&Policy{
		Type:     TypeExec,
		Interval: "1h",
		Exec:     &ExecCheck{Command: []string{"true"}},
	})
	require.NoError(t, err)

	previousCheckedAt := time.Date(2026, 5, 16, 0, 0, 0, 0, time.UTC)
	startedAt := time.Date(2026, 5, 16, 1, 0, 0, 0, time.UTC)
	store := &controllerTestStore{
		events:   make(chan InstanceEvent),
		runtimes: make(chan *Runtime, 4),
	}
	controller := NewController(store, controllerTestRunner{result: ProbeResult{Success: true}}, ControllerOptions{
		Now: func() time.Time { return startedAt },
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- controller.Run(ctx)
	}()

	store.events <- InstanceEvent{
		Action:     InstanceEventStart,
		InstanceID: "inst-1",
		Instance: &Instance{
			ID:              "inst-1",
			State:           StateRunning,
			StartedAt:       &startedAt,
			GuestAgentReady: true,
			HealthCheck:     policy,
			Runtime: &Runtime{
				Status:               StatusHealthy,
				StartedAt:            &previousCheckedAt,
				LastCheckedAt:        &previousCheckedAt,
				ConsecutiveSuccesses: 9,
			},
		},
	}

	var starting *Runtime
	deadline := time.After(time.Second)
	for starting == nil {
		select {
		case runtime := <-store.runtimes:
			if runtime.Status == StatusStarting {
				starting = runtime
			}
		case <-deadline:
			t.Fatal("timed out waiting for starting status")
		}
	}

	assert.Equal(t, startedAt, *starting.StartedAt)
	assert.Nil(t, starting.LastCheckedAt)
	assert.Zero(t, starting.ConsecutiveSuccesses)

	cancel()
	require.NoError(t, <-done)
}
