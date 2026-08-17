//go:build linux

package providers

import (
	"context"
	"testing"
	"time"

	"github.com/kernel/hypeman/lib/autostandby"
	"github.com/kernel/hypeman/lib/instances"
	"github.com/stretchr/testify/require"
)

type autoStandbyStateManagerStub struct {
	stateByID map[string]*autostandby.AutoStandbyState
	events    chan instances.LifecycleEvent
}

func (s *autoStandbyStateManagerStub) GetAutoStandbyState(_ context.Context, id string) (*autostandby.AutoStandbyState, error) {
	if autoStandbyState, ok := s.stateByID[id]; ok {
		cloned := *autoStandbyState
		return &cloned, nil
	}
	return nil, nil
}

func (s *autoStandbyStateManagerStub) SetAutoStandbyState(context.Context, string, *autostandby.AutoStandbyState) error {
	return nil
}

func (s *autoStandbyStateManagerStub) SubscribeLifecycleEvents(instances.LifecycleEventConsumer) (<-chan instances.LifecycleEvent, func()) {
	return s.events, func() {}
}

func TestAutoStandbyInstanceStoreSubscribeInstanceEventsIncludesState(t *testing.T) {
	t.Parallel()

	idleSince := time.Date(2026, 4, 7, 12, 0, 0, 0, time.UTC)
	lastInbound := idleSince.Add(-time.Minute)
	stateManager := &autoStandbyStateManagerStub{
		stateByID: map[string]*autostandby.AutoStandbyState{
			"inst-1": {
				IdleSince:             &idleSince,
				LastInboundActivityAt: &lastInbound,
			},
		},
		events: make(chan instances.LifecycleEvent, 1),
	}
	store := autoStandbyInstanceStore{
		stateManager: stateManager,
	}

	eventCh, _, err := store.SubscribeInstanceEvents()
	require.NoError(t, err)

	stateManager.events <- instances.LifecycleEvent{
		Action:     instances.LifecycleEventUpdate,
		InstanceID: "inst-1",
		Instance: &instances.Instance{
			StoredMetadata: instances.StoredMetadata{
				Id:             "inst-1",
				Name:           "inst-1",
				NetworkEnabled: true,
				IP:             "192.168.100.20",
				AutoStandby:    &autostandby.Policy{Enabled: true, IdleTimeout: "5m"},
			},
			State: instances.StateRunning,
		},
	}

	event := <-eventCh
	require.NotNil(t, event.Instance)
	require.NotNil(t, event.Instance.AutoStandbyState)
	require.NotNil(t, event.Instance.AutoStandbyState.IdleSince)
	require.Equal(t, idleSince, *event.Instance.AutoStandbyState.IdleSince)
	require.NotNil(t, event.Instance.AutoStandbyState.LastInboundActivityAt)
	require.Equal(t, lastInbound, *event.Instance.AutoStandbyState.LastInboundActivityAt)
}
