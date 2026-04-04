package autostandby

import (
	"context"
	"net/netip"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeInstanceStore struct {
	instances  []Instance
	standbyIDs []string
	standbyErr error
}

func (f *fakeInstanceStore) ListInstances(context.Context) ([]Instance, error) {
	return append([]Instance(nil), f.instances...), nil
}

func (f *fakeInstanceStore) StandbyInstance(_ context.Context, id string) error {
	f.standbyIDs = append(f.standbyIDs, id)
	return f.standbyErr
}

type fakeConnectionSource struct {
	connections []Connection
	err         error
}

func (f *fakeConnectionSource) ListConnections(context.Context) ([]Connection, error) {
	if f.err != nil {
		return nil, f.err
	}
	return append([]Connection(nil), f.connections...), nil
}

func TestControllerWaitsFullIdleTimeoutFromStartup(t *testing.T) {
	t.Parallel()

	store := &fakeInstanceStore{
		instances: []Instance{{
			ID:             "inst-idle",
			Name:           "inst-idle",
			State:          StateRunning,
			NetworkEnabled: true,
			IP:             "192.168.100.10",
			AutoStandby: &Policy{
				Enabled:     true,
				IdleTimeout: "5m",
			},
		}},
	}
	source := &fakeConnectionSource{}
	controller := NewController(store, source, nil, 0)

	now := time.Date(2026, 4, 3, 12, 0, 0, 0, time.UTC)
	controller.now = func() time.Time { return now }

	require.NoError(t, controller.Poll(context.Background()))
	assert.Empty(t, store.standbyIDs)

	now = now.Add(4 * time.Minute)
	require.NoError(t, controller.Poll(context.Background()))
	assert.Empty(t, store.standbyIDs)

	now = now.Add(1 * time.Minute)
	require.NoError(t, controller.Poll(context.Background()))
	assert.Equal(t, []string{"inst-idle"}, store.standbyIDs)
}

func TestControllerClearsIdleTimerWhenTrafficReturns(t *testing.T) {
	t.Parallel()

	store := &fakeInstanceStore{
		instances: []Instance{{
			ID:             "inst-busy",
			Name:           "inst-busy",
			State:          StateRunning,
			NetworkEnabled: true,
			IP:             "192.168.100.20",
			AutoStandby: &Policy{
				Enabled:     true,
				IdleTimeout: "1m",
			},
		}},
	}
	source := &fakeConnectionSource{}
	controller := NewController(store, source, nil, 0)

	now := time.Date(2026, 4, 3, 13, 0, 0, 0, time.UTC)
	controller.now = func() time.Time { return now }

	require.NoError(t, controller.Poll(context.Background()))
	now = now.Add(30 * time.Second)
	source.connections = []Connection{{
		OriginalSourceIP:        mustAddr("1.2.3.4"),
		OriginalDestinationIP:   mustAddr("192.168.100.20"),
		OriginalDestinationPort: 8080,
		TCPState:                TCPStateEstablished,
	}}
	require.NoError(t, controller.Poll(context.Background()))

	now = now.Add(70 * time.Second)
	source.connections = nil
	require.NoError(t, controller.Poll(context.Background()))
	assert.Empty(t, store.standbyIDs)
}

func TestControllerSkipsIneligibleInstances(t *testing.T) {
	t.Parallel()

	store := &fakeInstanceStore{
		instances: []Instance{
			{ID: "stopped", State: "Stopped", NetworkEnabled: true, IP: "192.168.1.10", AutoStandby: &Policy{Enabled: true, IdleTimeout: "1m"}},
			{ID: "vgpu", State: StateRunning, NetworkEnabled: true, IP: "192.168.1.11", HasVGPU: true, AutoStandby: &Policy{Enabled: true, IdleTimeout: "1m"}},
		},
	}
	controller := NewController(store, &fakeConnectionSource{}, nil, 0)

	require.NoError(t, controller.Poll(context.Background()))
	assert.Empty(t, store.standbyIDs)
}

func mustAddr(raw string) netip.Addr {
	return netip.MustParseAddr(raw)
}
