package api

import (
	"context"
	"net/netip"
	"testing"
	"time"

	"github.com/kernel/hypeman/lib/autostandby"
	"github.com/kernel/hypeman/lib/instances"
	"github.com/kernel/hypeman/lib/oapi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type captureStatusManager struct {
	instances.Manager
	instance *instances.Instance
	err      error
}

func (m *captureStatusManager) GetInstance(context.Context, string) (*instances.Instance, error) {
	if m.err != nil {
		return nil, m.err
	}
	return m.instance, nil
}

type statusStore struct {
	instances []autostandby.Instance
	runtime   map[string]*autostandby.Runtime
	events    chan autostandby.InstanceEvent
}

func (s *statusStore) ListInstances(context.Context) ([]autostandby.Instance, error) {
	return append([]autostandby.Instance(nil), s.instances...), nil
}

func (s *statusStore) StandbyInstance(context.Context, string) error { return nil }

func (s *statusStore) SetRuntime(_ context.Context, id string, runtime *autostandby.Runtime) error {
	if s.runtime == nil {
		s.runtime = make(map[string]*autostandby.Runtime)
	}
	s.runtime[id] = runtime
	return nil
}

func (s *statusStore) SubscribeInstanceEvents() (<-chan autostandby.InstanceEvent, func(), error) {
	if s.events == nil {
		s.events = make(chan autostandby.InstanceEvent)
	}
	return s.events, func() {}, nil
}

type statusConnectionSource struct {
	connections []autostandby.Connection
}

func (s *statusConnectionSource) ListConnections(context.Context) ([]autostandby.Connection, error) {
	return append([]autostandby.Connection(nil), s.connections...), nil
}

func (s *statusConnectionSource) OpenStream(context.Context) (autostandby.ConnectionStream, error) {
	return &statusConnectionStream{
		events: make(chan autostandby.ConnectionEvent),
		errs:   make(chan error),
	}, nil
}

type statusConnectionStream struct {
	events chan autostandby.ConnectionEvent
	errs   chan error
}

func (s *statusConnectionStream) Events() <-chan autostandby.ConnectionEvent { return s.events }

func (s *statusConnectionStream) Errors() <-chan error { return s.errs }

func (s *statusConnectionStream) Close() error { return nil }

func TestGetAutoStandbyStatusUnsupportedWithoutController(t *testing.T) {
	t.Parallel()

	base := newTestService(t)
	base.InstanceManager = &captureStatusManager{
		Manager: base.InstanceManager,
		instance: &instances.Instance{
			StoredMetadata: instances.StoredMetadata{
				Id:             "inst-1",
				Name:           "inst-1",
				NetworkEnabled: true,
				IP:             "192.168.100.10",
				AutoStandby:    &autostandby.Policy{Enabled: true, IdleTimeout: "5m"},
			},
			State: instances.StateRunning,
		},
	}

	resp, err := base.GetAutoStandbyStatus(ctx(), oapi.GetAutoStandbyStatusRequestObject{Id: "inst-1"})
	require.NoError(t, err)

	statusResp, ok := resp.(oapi.GetAutoStandbyStatus200JSONResponse)
	require.True(t, ok)
	assert.False(t, statusResp.Supported)
	assert.Equal(t, oapi.AutoStandbyStatusStatusUnsupported, statusResp.Status)
	assert.Equal(t, oapi.AutoStandbyStatusReasonUnsupportedPlatform, statusResp.Reason)
}

func TestGetAutoStandbyStatusActive(t *testing.T) {
	t.Parallel()

	inst := &instances.Instance{
		StoredMetadata: instances.StoredMetadata{
			Id:             "inst-2",
			Name:           "inst-2",
			NetworkEnabled: true,
			IP:             "192.168.100.20",
			AutoStandby:    &autostandby.Policy{Enabled: true, IdleTimeout: "5m"},
		},
		State: instances.StateRunning,
	}

	now := time.Date(2026, 4, 6, 12, 0, 0, 0, time.UTC)
	store := &statusStore{
		instances: []autostandby.Instance{{
			ID:             "inst-2",
			Name:           "inst-2",
			State:          autostandby.StateRunning,
			NetworkEnabled: true,
			IP:             "192.168.100.20",
			AutoStandby:    &autostandby.Policy{Enabled: true, IdleTimeout: "5m"},
		}},
	}
	source := &statusConnectionSource{connections: []autostandby.Connection{{
		OriginalSourceIP:        mustStatusAddr("1.2.3.4"),
		OriginalSourcePort:      51234,
		OriginalDestinationIP:   mustStatusAddr("192.168.100.20"),
		OriginalDestinationPort: 8080,
		TCPState:                autostandby.TCPStateEstablished,
	}}}
	controller := autostandby.NewController(store, source, autostandby.ControllerOptions{
		Now: func() time.Time { return now },
	})
	require.NoError(t, controller.Run(withCanceledContext(t)))

	base := newTestService(t)
	base.InstanceManager = &captureStatusManager{Manager: base.InstanceManager, instance: inst}
	base.AutoStandbyController = controller

	resp, err := base.GetAutoStandbyStatus(ctx(), oapi.GetAutoStandbyStatusRequestObject{Id: "inst-2"})
	require.NoError(t, err)

	statusResp, ok := resp.(oapi.GetAutoStandbyStatus200JSONResponse)
	require.True(t, ok)
	assert.True(t, statusResp.Supported)
	assert.Equal(t, oapi.AutoStandbyStatusStatusActive, statusResp.Status)
	assert.Equal(t, 1, statusResp.ActiveInboundConnections)
}

func withCanceledContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}

func mustStatusAddr(raw string) netip.Addr {
	return netip.MustParseAddr(raw)
}
