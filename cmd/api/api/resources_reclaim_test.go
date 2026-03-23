package api

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/kernel/hypeman/lib/guestmemory"
	"github.com/kernel/hypeman/lib/hypervisor"
	"github.com/kernel/hypeman/lib/oapi"
	"github.com/stretchr/testify/require"
)

type stubGuestMemoryController struct {
	response guestmemory.ManualReclaimResponse
	err      error
	requests []guestmemory.ManualReclaimRequest
}

func (s *stubGuestMemoryController) Start(ctx context.Context) error {
	<-ctx.Done()
	return nil
}

func (s *stubGuestMemoryController) TriggerReclaim(ctx context.Context, req guestmemory.ManualReclaimRequest) (guestmemory.ManualReclaimResponse, error) {
	s.requests = append(s.requests, req)
	return s.response, s.err
}

func TestReclaimMemory_DefaultHoldAndResponse(t *testing.T) {
	controller := &stubGuestMemoryController{
		response: guestmemory.ManualReclaimResponse{
			RequestedReclaimBytes: 512 * 1024 * 1024,
			PlannedReclaimBytes:   512 * 1024 * 1024,
			AppliedReclaimBytes:   256 * 1024 * 1024,
			HostAvailableBytes:    2 * 1024 * 1024 * 1024,
			HostPressureState:     guestmemory.HostPressureStateHealthy,
			Actions: []guestmemory.ManualReclaimAction{
				{
					InstanceID:                     "inst-123",
					InstanceName:                   "guestmem-test",
					Hypervisor:                     hypervisor.TypeQEMU,
					AssignedMemoryBytes:            4 * 1024 * 1024 * 1024,
					ProtectedFloorBytes:            2 * 1024 * 1024 * 1024,
					PreviousTargetGuestMemoryBytes: 4 * 1024 * 1024 * 1024,
					PlannedTargetGuestMemoryBytes:  3 * 1024 * 1024 * 1024,
					TargetGuestMemoryBytes:         3758096384,
					AppliedReclaimBytes:            268435456,
					Status:                         "applied",
				},
			},
		},
	}

	svc := &ApiService{GuestMemoryController: controller}
	resp, err := svc.ReclaimMemory(context.Background(), oapi.ReclaimMemoryRequestObject{
		Body: &oapi.MemoryReclaimRequest{
			ReclaimBytes: 512 * 1024 * 1024,
			Reason:       ptr("pack host before launch"),
		},
	})
	require.NoError(t, err)

	okResp, ok := resp.(oapi.ReclaimMemory200JSONResponse)
	require.True(t, ok)
	require.Len(t, controller.requests, 1)
	require.Equal(t, 5*time.Minute, controller.requests[0].HoldFor)
	require.Equal(t, "pack host before launch", controller.requests[0].Reason)
	require.Equal(t, int64(512*1024*1024), okResp.RequestedReclaimBytes)
	require.Equal(t, oapi.MemoryReclaimResponseHostPressureState(guestmemory.HostPressureStateHealthy), okResp.HostPressureState)
	require.Len(t, okResp.Actions, 1)
	require.Equal(t, oapi.MemoryReclaimActionHypervisor(hypervisor.TypeQEMU), okResp.Actions[0].Hypervisor)
}

func TestReclaimMemory_ValidationAndFeatureDisabled(t *testing.T) {
	svc := &ApiService{GuestMemoryController: &stubGuestMemoryController{err: guestmemory.ErrActiveBallooningDisabled}}
	resp, err := svc.ReclaimMemory(context.Background(), oapi.ReclaimMemoryRequestObject{
		Body: &oapi.MemoryReclaimRequest{
			ReclaimBytes: 256 * 1024 * 1024,
			HoldFor:      ptr("2h"),
		},
	})
	require.NoError(t, err)

	badReq, ok := resp.(oapi.ReclaimMemory400JSONResponse)
	require.True(t, ok)
	require.Equal(t, "bad_request", badReq.Code)

	resp, err = svc.ReclaimMemory(context.Background(), oapi.ReclaimMemoryRequestObject{
		Body: &oapi.MemoryReclaimRequest{
			ReclaimBytes: 256 * 1024 * 1024,
			HoldFor:      ptr("10m"),
		},
	})
	require.NoError(t, err)

	featureDisabled, ok := resp.(oapi.ReclaimMemory400JSONResponse)
	require.True(t, ok)
	require.Equal(t, "feature_disabled", featureDisabled.Code)
}

func TestReclaimMemory_InternalError(t *testing.T) {
	svc := &ApiService{GuestMemoryController: &stubGuestMemoryController{err: errors.New("boom")}}
	resp, err := svc.ReclaimMemory(context.Background(), oapi.ReclaimMemoryRequestObject{
		Body: &oapi.MemoryReclaimRequest{ReclaimBytes: 128 * 1024 * 1024},
	})
	require.NoError(t, err)

	internalErr, ok := resp.(oapi.ReclaimMemory500JSONResponse)
	require.True(t, ok)
	require.Equal(t, "internal_error", internalErr.Code)
}

func ptr(v string) *string {
	return &v
}
