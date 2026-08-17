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

func TestHoldAutoStandbyExtendsCountdown(t *testing.T) {
	t.Parallel()

	inst := &instances.Instance{
		StoredMetadata: instances.StoredMetadata{
			Id:             "inst-hold",
			Name:           "inst-hold",
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
			ID:               "inst-hold",
			Name:             "inst-hold",
			State:            autostandby.StateRunning,
			NetworkEnabled:   true,
			IP:               "192.168.100.30",
			AutoStandby:      &autostandby.Policy{Enabled: true, IdleTimeout: "5m"},
			AutoStandbyState: &autostandby.AutoStandbyState{IdleSince: &idleSince},
		}},
	}
	controller := autostandby.NewController(store, &statusConnectionSource{}, autostandby.ControllerOptions{
		Now: func() time.Time { return now },
	})
	require.NoError(t, controller.Run(withCanceledContext(t)))

	base := newTestService(t)
	base.InstanceManager = &captureStatusManager{Manager: base.InstanceManager, instance: inst}
	base.AutoStandbyController = controller

	resp, err := base.HoldAutoStandby(ctxWithInstance(base, "inst-hold"), oapi.HoldAutoStandbyRequestObject{Id: "inst-hold"})
	require.NoError(t, err)

	holdResp, ok := resp.(oapi.HoldAutoStandby200JSONResponse)
	require.True(t, ok)
	assert.Equal(t, oapi.AutoStandbyStatusStatusIdleCountdown, holdResp.Status)
	require.NotNil(t, holdResp.HoldUntil)
	assert.Equal(t, now.Add(5*time.Minute), *holdResp.HoldUntil)
	require.NotNil(t, holdResp.NextStandbyAt)
	assert.Equal(t, now.Add(5*time.Minute), *holdResp.NextStandbyAt)
}

func TestHoldAutoStandbyConflictWhenInstanceInStandby(t *testing.T) {
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

	resp, err := base.HoldAutoStandby(ctxWithInstance(base, "inst-standby"), oapi.HoldAutoStandbyRequestObject{Id: "inst-standby"})
	require.NoError(t, err)

	conflictResp, ok := resp.(oapi.HoldAutoStandby409JSONResponse)
	require.True(t, ok)
	assert.Equal(t, "instance_in_standby", conflictResp.Code)
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

func TestHoldAutoStandbyConflictWhenStandbyCompletesDuringHold(t *testing.T) {
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

	controller := autostandby.NewController(&statusStore{}, &statusConnectionSource{}, autostandby.ControllerOptions{})
	require.NoError(t, controller.Run(withCanceledContext(t)))

	base := newTestService(t)
	base.InstanceManager = &sequenceManager{Manager: base.InstanceManager, sequence: []*instances.Instance{running, standby}}
	base.AutoStandbyController = controller

	resp, err := base.HoldAutoStandby(ctxWithInstance(base, "inst-race"), oapi.HoldAutoStandbyRequestObject{Id: "inst-race"})
	require.NoError(t, err)

	conflictResp, ok := resp.(oapi.HoldAutoStandby409JSONResponse)
	require.True(t, ok)
	assert.Equal(t, "instance_in_standby", conflictResp.Code)
}

func TestHoldAutoStandbyUnsupportedWithoutController(t *testing.T) {
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

	resp, err := base.HoldAutoStandby(ctxWithInstance(base, "inst-nosupport"), oapi.HoldAutoStandbyRequestObject{Id: "inst-nosupport"})
	require.NoError(t, err)

	holdResp, ok := resp.(oapi.HoldAutoStandby200JSONResponse)
	require.True(t, ok)
	assert.False(t, holdResp.Supported)
	assert.Equal(t, oapi.AutoStandbyStatusStatusUnsupported, holdResp.Status)
}
