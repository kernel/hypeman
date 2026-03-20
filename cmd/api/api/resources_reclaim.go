package api

import (
	"context"
	"errors"
	"time"

	"github.com/kernel/hypeman/lib/guestmemory"
	"github.com/kernel/hypeman/lib/logger"
	"github.com/kernel/hypeman/lib/oapi"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

const (
	defaultMemoryReclaimHold = 5 * time.Minute
	maxMemoryReclaimHold     = 1 * time.Hour
)

// ReclaimMemory triggers proactive guest memory reclaim via runtime ballooning.
func (s *ApiService) ReclaimMemory(ctx context.Context, request oapi.ReclaimMemoryRequestObject) (oapi.ReclaimMemoryResponseObject, error) {
	log := logger.FromContext(ctx)
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

	tracer := otel.Tracer("hypeman/guestmemory")
	ctx, span := tracer.Start(ctx, "guestmemory.manual_reclaim",
		traceAttrsForManualReclaim(request.Body.ReclaimBytes, holdFor, request.Body.DryRun != nil && *request.Body.DryRun, request.Body.Reason != nil))
	defer span.End()

	log.InfoContext(ctx,
		"manual guest memory reclaim requested",
		"operation", "manual_reclaim",
		"requested_reclaim_bytes", request.Body.ReclaimBytes,
		"hold_for_seconds", holdFor.Seconds(),
		"dry_run", request.Body.DryRun != nil && *request.Body.DryRun,
		"reason_present", request.Body.Reason != nil,
	)

	resp, err := s.GuestMemoryController.TriggerReclaim(ctx, guestmemory.ManualReclaimRequest{
		ReclaimBytes: request.Body.ReclaimBytes,
		HoldFor:      holdFor,
		DryRun:       request.Body.DryRun != nil && *request.Body.DryRun,
		Reason:       derefString(request.Body.Reason),
	})
	if err != nil {
		switch {
		case errors.Is(err, guestmemory.ErrGuestMemoryDisabled), errors.Is(err, guestmemory.ErrActiveBallooningDisabled):
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
			log.WarnContext(ctx, "manual guest memory reclaim rejected", "operation", "manual_reclaim", "error", err)
			return oapi.ReclaimMemory400JSONResponse{
				Code:    "feature_disabled",
				Message: err.Error(),
			}, nil
		default:
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
			log.ErrorContext(ctx, "manual guest memory reclaim failed", "operation", "manual_reclaim", "error", err)
			return oapi.ReclaimMemory500JSONResponse{
				Code:    "internal_error",
				Message: err.Error(),
			}, nil
		}
	}

	span.SetAttributes(
		attribute.Int64("planned_reclaim_bytes", resp.PlannedReclaimBytes),
		attribute.Int64("applied_reclaim_bytes", resp.AppliedReclaimBytes),
		attribute.Int64("host_available_bytes", resp.HostAvailableBytes),
		attribute.String("host_pressure_state", string(resp.HostPressureState)),
		attribute.Int("action_count", len(resp.Actions)),
	)
	span.SetStatus(codes.Ok, "")
	log.InfoContext(ctx,
		"manual guest memory reclaim completed",
		"operation", "manual_reclaim",
		"planned_reclaim_bytes", resp.PlannedReclaimBytes,
		"applied_reclaim_bytes", resp.AppliedReclaimBytes,
		"host_available_bytes", resp.HostAvailableBytes,
		"host_pressure_state", resp.HostPressureState,
		"action_count", len(resp.Actions),
	)

	return oapi.ReclaimMemory200JSONResponse(memoryReclaimResponseToOAPI(resp)), nil
}

func traceAttrsForManualReclaim(reclaimBytes int64, holdFor time.Duration, dryRun bool, reasonPresent bool) trace.SpanStartOption {
	return trace.WithAttributes(
		attribute.Int64("requested_reclaim_bytes", reclaimBytes),
		attribute.Float64("hold_for_seconds", holdFor.Seconds()),
		attribute.Bool("dry_run", dryRun),
		attribute.Bool("reason_present", reasonPresent),
	)
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
