package api

import (
	"context"
	"testing"
	"time"

	"github.com/kernel/hypeman/lib/hypervisor"
	"github.com/kernel/hypeman/lib/instances"
	"github.com/kernel/hypeman/lib/oapi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type captureForkSnapshotManager struct {
	instances.Manager
	lastID  string
	lastReq *instances.ForkSnapshotRequest
	result  *instances.Instance
	err     error
}

func (m *captureForkSnapshotManager) ForkSnapshot(ctx context.Context, snapshotID string, req instances.ForkSnapshotRequest) (*instances.Instance, error) {
	reqCopy := req
	m.lastID = snapshotID
	m.lastReq = &reqCopy
	if m.err != nil {
		return nil, m.err
	}
	return m.result, nil
}

func TestSnapshotScheduleToOAPIPreservesZeroMaxCount(t *testing.T) {
	t.Parallel()

	schedule := instances.SnapshotSchedule{
		InstanceID: "inst-1",
		Interval:   time.Hour,
		Retention: instances.SnapshotScheduleRetention{
			MaxCount: 0,
			MaxAge:   24 * time.Hour,
		},
		NextRunAt: time.Now().UTC().Add(time.Hour),
		CreatedAt: time.Now().UTC(),
		UpdatedAt: time.Now().UTC(),
	}

	out := snapshotScheduleToOAPI(schedule)
	require.NotNil(t, out.Retention.MaxCount)
	assert.Equal(t, 0, *out.Retention.MaxCount)
	require.NotNil(t, out.Retention.MaxAge)
	assert.Equal(t, "24h0m0s", *out.Retention.MaxAge)
}

func TestForkSnapshotMapsWaitForNetwork(t *testing.T) {
	t.Parallel()
	svc := newTestService(t)

	forked := &instances.Instance{
		StoredMetadata: instances.StoredMetadata{
			Id:             "forked-instance",
			Name:           "forked-instance",
			Image:          "docker.io/library/alpine:latest",
			CreatedAt:      time.Now(),
			HypervisorType: hypervisor.TypeFirecracker,
		},
		State: instances.StateRunning,
	}
	mockMgr := &captureForkSnapshotManager{
		Manager: svc.InstanceManager,
		result:  forked,
	}
	svc.InstanceManager = mockMgr
	waitForNetwork := false

	resp, err := svc.ForkSnapshot(ctx(), oapi.ForkSnapshotRequestObject{
		SnapshotId: "snap-123",
		Body: &oapi.ForkSnapshotRequest{
			Name:           "forked-instance",
			WaitForNetwork: &waitForNetwork,
		},
	})
	require.NoError(t, err)

	created, ok := resp.(oapi.ForkSnapshot201JSONResponse)
	require.True(t, ok, "expected 201 response")
	assert.Equal(t, "forked-instance", created.Name)
	assert.Equal(t, "snap-123", mockMgr.lastID)
	require.NotNil(t, mockMgr.lastReq)
	assert.Equal(t, "forked-instance", mockMgr.lastReq.Name)
	require.NotNil(t, mockMgr.lastReq.WaitForNetwork)
	assert.False(t, *mockMgr.lastReq.WaitForNetwork)
}
