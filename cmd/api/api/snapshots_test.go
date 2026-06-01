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
	waitForAck := true
	ackTimeoutMS := 2500

	resp, err := svc.ForkSnapshot(ctx(), oapi.ForkSnapshotRequestObject{
		SnapshotId: "snap-123",
		Body: &oapi.ForkSnapshotRequest{
			Name:           "forked-instance",
			WaitForNetwork: &waitForNetwork,
			Mailboxes: &[]oapi.ForkMailboxPayload{{
				Name:         "kernel.identity.v1",
				Token:        "template-token",
				Payload:      map[string]interface{}{"instance_name": "forked-instance"},
				WaitForAck:   &waitForAck,
				AckTimeoutMs: &ackTimeoutMS,
			}},
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
	require.Len(t, mockMgr.lastReq.Mailboxes, 1)
	assert.Equal(t, "kernel.identity.v1", mockMgr.lastReq.Mailboxes[0].Name)
	assert.Equal(t, "template-token", mockMgr.lastReq.Mailboxes[0].Token)
	assert.True(t, mockMgr.lastReq.Mailboxes[0].WaitForAck)
	assert.Equal(t, 2500*time.Millisecond, mockMgr.lastReq.Mailboxes[0].AckTimeout)
	assert.JSONEq(t, `{"instance_name":"forked-instance"}`, string(mockMgr.lastReq.Mailboxes[0].Payload))
}
