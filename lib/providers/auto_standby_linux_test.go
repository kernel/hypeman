//go:build linux

package providers

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/kernel/hypeman/lib/autostandby"
	"github.com/kernel/hypeman/lib/instances"
	"github.com/stretchr/testify/require"
)

type autoStandbyRuntimeManagerStub struct {
	runtimeByID map[string]*autostandby.Runtime
	errByID     map[string]error
	events      chan instances.LifecycleEvent
}

func (s *autoStandbyRuntimeManagerStub) GetAutoStandbyRuntime(_ context.Context, id string) (*autostandby.Runtime, error) {
	if err, ok := s.errByID[id]; ok {
		return nil, err
	}
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

type autoStandbyListManagerStub struct {
	instances.Manager
	insts []instances.Instance
}

func (s autoStandbyListManagerStub) ListInstances(context.Context, *instances.ListInstancesFilter) ([]instances.Instance, error) {
	return s.insts, nil
}

func TestAutoStandbyInstanceStoreListToleratesUnknownInstanceWithoutRuntime(t *testing.T) {
	t.Parallel()

	idleSince := time.Date(2026, 4, 7, 12, 0, 0, 0, time.UTC)
	unreadable := errors.New("unmarshal metadata: unexpected end of JSON input")
	stateError := unreadable.Error()
	store := autoStandbyInstanceStore{
		manager: autoStandbyListManagerStub{insts: []instances.Instance{
			{
				StoredMetadata: instances.StoredMetadata{Id: "running", Name: "running", NetworkEnabled: true},
				State:          instances.StateRunning,
			},
			{
				// Unknown because the VMM query failed; metadata is still readable.
				StoredMetadata: instances.StoredMetadata{Id: "unknown-vmm"},
				State:          instances.StateUnknown,
			},
			{
				StoredMetadata: instances.StoredMetadata{Id: "corrupt"},
				State:          instances.StateUnknown,
				StateError:     &stateError,
			},
		}},
		runtimeManager: &autoStandbyRuntimeManagerStub{
			runtimeByID: map[string]*autostandby.Runtime{
				"running":     {IdleSince: &idleSince},
				"unknown-vmm": {IdleSince: &idleSince},
			},
			errByID: map[string]error{"corrupt": unreadable},
		},
	}

	listed, err := store.ListInstances(context.Background())
	require.NoError(t, err)
	require.Len(t, listed, 3)

	require.Equal(t, "running", listed[0].ID)
	require.NotNil(t, listed[0].Runtime)
	require.Equal(t, idleSince, *listed[0].Runtime.IdleSince)

	require.Equal(t, "unknown-vmm", listed[1].ID)
	require.Equal(t, string(instances.StateUnknown), listed[1].State)
	require.NotNil(t, listed[1].Runtime, "readable metadata keeps its runtime even when state is Unknown")

	require.Equal(t, "corrupt", listed[2].ID)
	require.Equal(t, string(instances.StateUnknown), listed[2].State)
	require.Nil(t, listed[2].Runtime)
}

func TestAutoStandbyInstanceStoreListFailsOnRuntimeErrorForKnownState(t *testing.T) {
	t.Parallel()

	loadErr := errors.New("read metadata: permission denied")
	store := autoStandbyInstanceStore{
		manager: autoStandbyListManagerStub{insts: []instances.Instance{{
			StoredMetadata: instances.StoredMetadata{Id: "running"},
			State:          instances.StateRunning,
		}}},
		runtimeManager: &autoStandbyRuntimeManagerStub{
			errByID: map[string]error{"running": loadErr},
		},
	}

	_, err := store.ListInstances(context.Background())
	require.ErrorIs(t, err, loadErr)
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
