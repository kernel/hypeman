package restartpolicy

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeStore struct {
	instances []Instance
	started   []string
	statuses  map[string]Status
	startErr  error
}

func (s *fakeStore) ListInstances(context.Context) ([]Instance, error) {
	out := make([]Instance, len(s.instances))
	copy(out, s.instances)
	return out, nil
}

func (s *fakeStore) RestartInstance(_ context.Context, id string) error {
	s.started = append(s.started, id)
	return s.startErr
}

func (s *fakeStore) SetRestartStatus(_ context.Context, id string, status Status) error {
	if s.statuses == nil {
		s.statuses = make(map[string]Status)
	}
	s.statuses[id] = status
	return nil
}

func (s *fakeStore) SubscribeInstanceEvents() (<-chan InstanceEvent, func(), error) {
	ch := make(chan InstanceEvent)
	close(ch)
	return ch, func() {}, nil
}

func TestReconcileRestartsFailedStoppedInstance(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	exitCode := 1
	store := &fakeStore{
		instances: []Instance{{
			ID:            "inst-1",
			State:         StateStopped,
			ExitCode:      &exitCode,
			RestartPolicy: &Policy{Policy: PolicyOnFailure},
		}},
	}
	controller := NewController(store, ControllerOptions{Now: func() time.Time { return now }})

	require.NoError(t, controller.Reconcile(context.Background()))

	assert.Equal(t, []string{"inst-1"}, store.started)
	status := store.statuses["inst-1"]
	assert.Equal(t, 1, status.Attempts)
	require.NotNil(t, status.LastAttemptAt)
	assert.Equal(t, now, *status.LastAttemptAt)
}

func TestReconcileSkipsManualStop(t *testing.T) {
	store := &fakeStore{
		instances: []Instance{{
			ID:            "inst-1",
			State:         StateStopped,
			RestartPolicy: &Policy{Policy: PolicyAlways},
			RestartStatus: Status{BlockedReason: BlockedReasonManualStop},
		}},
	}
	controller := NewController(store, ControllerOptions{})

	require.NoError(t, controller.Reconcile(context.Background()))

	assert.Empty(t, store.started)
	assert.Nil(t, store.statuses)
}

func TestReconcileBlocksAfterMaxAttempts(t *testing.T) {
	store := &fakeStore{
		instances: []Instance{{
			ID:            "inst-1",
			State:         StateStopped,
			RestartPolicy: &Policy{Policy: PolicyAlways, MaxAttempts: 2},
			RestartStatus: Status{Attempts: 2},
		}},
	}
	controller := NewController(store, ControllerOptions{})

	require.NoError(t, controller.Reconcile(context.Background()))

	assert.Empty(t, store.started)
	assert.Equal(t, BlockedReasonMaxAttemptsExceeded, store.statuses["inst-1"].BlockedReason)
}

func TestReconcileSkipsCleanOnFailureExit(t *testing.T) {
	exitCode := 0
	store := &fakeStore{
		instances: []Instance{{
			ID:            "inst-1",
			State:         StateStopped,
			ExitCode:      &exitCode,
			RestartPolicy: &Policy{Policy: PolicyOnFailure},
		}},
	}
	controller := NewController(store, ControllerOptions{})

	require.NoError(t, controller.Reconcile(context.Background()))

	assert.Empty(t, store.started)
	assert.Nil(t, store.statuses)
}
