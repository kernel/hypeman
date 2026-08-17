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

type autoStandbyRuntimeManagerStub struct {
	runtimeByID map[string]*autostandby.Runtime
	events      chan instances.LifecycleEvent
}

func (s *autoStandbyRuntimeManagerStub) GetAutoStandbyRuntime(_ context.Context, id string) (*autostandby.Runtime, error) {
	if runtime, ok := s.runtimeByID[id]; ok {
		cloned := *runtime
		return &cloned, nil
	}
	return nil, nil
}

func (s *autoStandbyRuntimeManagerStub) SetAutoStandbyRuntime(context.Context, string, *autostandby.Runtime) error {
	return nil
}

func (s *autoStandbyRuntimeManagerStub) SubscribeLifecycleEvents(instances.LifecycleEventConsumer) (<-chan instances.LifecycleEvent, func()) {
	return s.events, func() {}
}

func TestAutoStandbyInstanceStoreSubscribeInstanceEventsIncludesRuntime(t *testing.T) {
	t.Parallel()

	idleSince := time.Date(2026, 4, 7, 12, 0, 0, 0, time.UTC)
	lastInbound := idleSince.Add(-time.Minute)
	runtimeManager := &autoStandbyRuntimeManagerStub{
		runtimeByID: map[string]*autostandby.Runtime{
			"inst-1": {
				IdleSince:             &idleSince,
				LastInboundActivityAt: &lastInbound,
			},
		},
		events: make(chan instances.LifecycleEvent, 1),
	}
	store := autoStandbyInstanceStore{
		runtimeManager: runtimeManager,
	}

	eventCh, _, err := store.SubscribeInstanceEvents()
	require.NoError(t, err)

	runtimeManager.events <- instances.LifecycleEvent{
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
	require.NotNil(t, event.Instance.Runtime)
	require.NotNil(t, event.Instance.Runtime.IdleSince)
	require.Equal(t, idleSince, *event.Instance.Runtime.IdleSince)
	require.NotNil(t, event.Instance.Runtime.LastInboundActivityAt)
	require.Equal(t, lastInbound, *event.Instance.Runtime.LastInboundActivityAt)
}
