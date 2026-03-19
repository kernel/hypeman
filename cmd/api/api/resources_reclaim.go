package api

import (
	"context"
	"errors"
	"time"

	"github.com/kernel/hypeman/lib/guestmemory"
	"github.com/kernel/hypeman/lib/oapi"
)

const (
	defaultMemoryReclaimHold = 5 * time.Minute
	maxMemoryReclaimHold     = 1 * time.Hour
)

// ReclaimMemory triggers proactive guest memory reclaim via runtime ballooning.
func (s *ApiService) ReclaimMemory(ctx context.Context, request oapi.ReclaimMemoryRequestObject) (oapi.ReclaimMemoryResponseObject, error) {
	if request.Body == nil {
		return oapi.ReclaimMemory400JSONResponse{
			Code:    "bad_request",
			Message: "request body is required",
		}, nil
	}
	if s.GuestMemoryController == nil {
		return oapi.ReclaimMemory500JSONResponse{
			Code:    "internal_error",
			Message: "guest memory controller not initialized",
		}, nil
	}

	holdFor, err := parseMemoryReclaimHold(request.Body)
	if err != nil {
		return oapi.ReclaimMemory400JSONResponse{
			Code:    "bad_request",
			Message: err.Error(),
		}, nil
	}

	resp, err := s.GuestMemoryController.TriggerReclaim(ctx, guestmemory.ManualReclaimRequest{
		ReclaimBytes: request.Body.ReclaimBytes,
		HoldFor:      holdFor,
		DryRun:       request.Body.DryRun != nil && *request.Body.DryRun,
		Reason:       derefString(request.Body.Reason),
	})
	if err != nil {
		switch {
		case errors.Is(err, guestmemory.ErrGuestMemoryDisabled), errors.Is(err, guestmemory.ErrActiveBallooningDisabled):
			return oapi.ReclaimMemory400JSONResponse{
				Code:    "feature_disabled",
				Message: err.Error(),
			}, nil
		default:
			return oapi.ReclaimMemory500JSONResponse{
				Code:    "internal_error",
				Message: err.Error(),
			}, nil
		}
	}

	return oapi.ReclaimMemory200JSONResponse(memoryReclaimResponseToOAPI(resp)), nil
}

func parseMemoryReclaimHold(req *oapi.MemoryReclaimRequest) (time.Duration, error) {
	if req == nil {
		return 0, nil
	}

	if req.HoldFor == nil {
		if req.ReclaimBytes > 0 {
			return defaultMemoryReclaimHold, nil
		}
		return 0, nil
	}

	holdFor, err := time.ParseDuration(*req.HoldFor)
	if err != nil {
		return 0, errors.New("hold_for must be a valid duration")
	}
	if holdFor < 0 {
		return 0, errors.New("hold_for must be non-negative")
	}
	if holdFor > maxMemoryReclaimHold {
		return 0, errors.New("hold_for must be less than or equal to 1h")
	}
	return holdFor, nil
}

func memoryReclaimResponseToOAPI(resp guestmemory.ManualReclaimResponse) oapi.MemoryReclaimResponse {
	out := oapi.MemoryReclaimResponse{
		RequestedReclaimBytes: resp.RequestedReclaimBytes,
		PlannedReclaimBytes:   resp.PlannedReclaimBytes,
		AppliedReclaimBytes:   resp.AppliedReclaimBytes,
		HoldUntil:             resp.HoldUntil,
		HostAvailableBytes:    resp.HostAvailableBytes,
		HostPressureState:     oapi.MemoryReclaimResponseHostPressureState(resp.HostPressureState),
		Actions:               make([]oapi.MemoryReclaimAction, 0, len(resp.Actions)),
	}

	for _, action := range resp.Actions {
		item := oapi.MemoryReclaimAction{
			InstanceId:                     action.InstanceID,
			InstanceName:                   action.InstanceName,
			Hypervisor:                     oapi.MemoryReclaimActionHypervisor(action.Hypervisor),
			AssignedMemoryBytes:            action.AssignedMemoryBytes,
			ProtectedFloorBytes:            action.ProtectedFloorBytes,
			PreviousTargetGuestMemoryBytes: action.PreviousTargetGuestMemoryBytes,
			PlannedTargetGuestMemoryBytes:  action.PlannedTargetGuestMemoryBytes,
			TargetGuestMemoryBytes:         action.TargetGuestMemoryBytes,
			AppliedReclaimBytes:            action.AppliedReclaimBytes,
			Status:                         action.Status,
		}
		if action.Error != "" {
			item.Error = &action.Error
		}
		out.Actions = append(out.Actions, item)
	}

	return out
}

func derefString(v *string) string {
	if v == nil {
		return ""
	}
	return *v
}
