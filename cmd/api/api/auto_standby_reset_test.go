package api

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/kernel/hypeman/lib/autostandby"
	"github.com/kernel/hypeman/lib/instances"
	"github.com/kernel/hypeman/lib/oapi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestResetAutoStandbyExtendsCountdown(t *testing.T) {
	t.Parallel()

	inst := &instances.Instance{
		StoredMetadata: instances.StoredMetadata{
			Id:             "inst-reset",
			Name:           "inst-reset",
			NetworkEnabled: true,
			IP:             "192.168.100.30",
			AutoStandby:    &autostandby.Policy{Enabled: true, IdleTimeout: "5m"},
		},
		State: instances.StateRunning,
	}

	now := time.Date(2026, 4, 6, 12, 0, 0, 0, time.UTC)
	idleSince := now.Add(-4 * time.Minute)
	store := &statusStore{
		instances: []autostandby.Instance{{
			ID:             "inst-reset",
			Name:           "inst-reset",
			State:          autostandby.StateRunning,
			NetworkEnabled: true,
			IP:             "192.168.100.30",
			AutoStandby:    &autostandby.Policy{Enabled: true, IdleTimeout: "5m"},
			Runtime:        &autostandby.Runtime{IdleSince: &idleSince},
		}},
	}
	controller := autostandby.NewController(store, &statusConnectionSource{}, autostandby.ControllerOptions{
		Now: func() time.Time { return now },
	})
	require.NoError(t, controller.Run(withCanceledContext(t)))

	base := newTestService(t)
	base.InstanceManager = &captureStatusManager{Manager: base.InstanceManager, instance: inst}
	base.AutoStandbyController = controller

	resp, err := base.ResetAutoStandby(ctx(), oapi.ResetAutoStandbyRequestObject{Id: "inst-reset"})
	require.NoError(t, err)

	resetResp, ok := resp.(oapi.ResetAutoStandby200JSONResponse)
	require.True(t, ok)
	assert.Equal(t, oapi.AutoStandbyStatusStatusIdleCountdown, resetResp.Status)
	require.NotNil(t, resetResp.NextStandbyAt)
	assert.Equal(t, now.Add(5*time.Minute), *resetResp.NextStandbyAt)

	require.NotNil(t, store.runtime["inst-reset"])
	require.NotNil(t, store.runtime["inst-reset"].IdleSince)
	assert.Equal(t, now, *store.runtime["inst-reset"].IdleSince)
}

func TestResetAutoStandbyConflictWhenInstanceInStandby(t *testing.T) {
	t.Parallel()

	base := newTestService(t)
	base.InstanceManager = &captureStatusManager{
		Manager: base.InstanceManager,
		instance: &instances.Instance{
			StoredMetadata: instances.StoredMetadata{
				Id:   "inst-standby",
				Name: "inst-standby",
			},
			State: instances.StateStandby,
		},
	}

	resp, err := base.ResetAutoStandby(ctx(), oapi.ResetAutoStandbyRequestObject{Id: "inst-standby"})
	require.NoError(t, err)

	conflictResp, ok := resp.(oapi.ResetAutoStandby409JSONResponse)
	require.True(t, ok)
	assert.Equal(t, "instance_in_standby", conflictResp.Code)
}

func TestResetAutoStandbyUnsupportedWithoutController(t *testing.T) {
	t.Parallel()

	base := newTestService(t)
	base.InstanceManager = &captureStatusManager{
		Manager: base.InstanceManager,
		instance: &instances.Instance{
			StoredMetadata: instances.StoredMetadata{
				Id:             "inst-nosupport",
				Name:           "inst-nosupport",
				NetworkEnabled: true,
				IP:             "192.168.100.31",
				AutoStandby:    &autostandby.Policy{Enabled: true, IdleTimeout: "5m"},
			},
			State: instances.StateRunning,
		},
	}

	resp, err := base.ResetAutoStandby(ctx(), oapi.ResetAutoStandbyRequestObject{Id: "inst-nosupport"})
	require.NoError(t, err)

	resetResp, ok := resp.(oapi.ResetAutoStandby200JSONResponse)
	require.True(t, ok)
	assert.False(t, resetResp.Supported)
	assert.Equal(t, oapi.AutoStandbyStatusStatusUnsupported, resetResp.Status)
}

// sequenceManager returns instances in order, mimicking state that changes
// between the handler's loads.
type sequenceManager struct {
	instances.Manager
	mu       sync.Mutex
	sequence []*instances.Instance
}

func (m *sequenceManager) GetInstance(context.Context, string) (*instances.Instance, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	inst := m.sequence[0]
	if len(m.sequence) > 1 {
		m.sequence = m.sequence[1:]
	}
	return inst, nil
}

func TestResetAutoStandbyConflictWhenStandbyCompletesDuringReset(t *testing.T) {
	t.Parallel()

	stored := instances.StoredMetadata{
		Id:             "inst-race",
		Name:           "inst-race",
		NetworkEnabled: true,
		IP:             "192.168.100.32",
		AutoStandby:    &autostandby.Policy{Enabled: true, IdleTimeout: "5m"},
	}
	running := &instances.Instance{StoredMetadata: stored, State: instances.StateRunning}
	standby := &instances.Instance{StoredMetadata: stored, State: instances.StateStandby}

	// The controller tracks nothing, mimicking state cleared by a standby that
	// completed between the handler's initial load and ResetIdle.
	controller := autostandby.NewController(&statusStore{}, &statusConnectionSource{}, autostandby.ControllerOptions{})
	require.NoError(t, controller.Run(withCanceledContext(t)))

	base := newTestService(t)
	base.InstanceManager = &sequenceManager{Manager: base.InstanceManager, sequence: []*instances.Instance{running, standby}}
	base.AutoStandbyController = controller

	resp, err := base.ResetAutoStandby(ctx(), oapi.ResetAutoStandbyRequestObject{Id: "inst-race"})
	require.NoError(t, err)

	conflictResp, ok := resp.(oapi.ResetAutoStandby409JSONResponse)
	require.True(t, ok)
	assert.Equal(t, "instance_in_standby", conflictResp.Code)
}
