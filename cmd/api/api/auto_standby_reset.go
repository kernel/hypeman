package api

import (
	"context"
	"errors"

	"github.com/kernel/hypeman/lib/autostandby"
	"github.com/kernel/hypeman/lib/instances"
	"github.com/kernel/hypeman/lib/logger"
	"github.com/kernel/hypeman/lib/oapi"
)

func (s *ApiService) ResetAutoStandby(ctx context.Context, request oapi.ResetAutoStandbyRequestObject) (oapi.ResetAutoStandbyResponseObject, error) {
	log := logger.FromContext(ctx)

	inst, err := s.InstanceManager.GetInstance(ctx, request.Id)
	if err != nil {
		if err == instances.ErrNotFound || err == instances.ErrAmbiguousName {
			return oapi.ResetAutoStandby404JSONResponse{
				Code:    "not_found",
				Message: "instance not found",
			}, nil
		}
		log.ErrorContext(ctx, "failed to resolve instance for auto-standby reset", "instance_id", request.Id, "error", err)
		return oapi.ResetAutoStandby500JSONResponse{
			Code:    "internal_error",
			Message: "failed to load instance",
		}, nil
	}

	var snapshot autostandby.StatusSnapshot
	if s.AutoStandbyController == nil {
		snapshot = autostandby.StatusSnapshot{
			Supported:    false,
			Configured:   inst.AutoStandby != nil,
			Enabled:      inst.AutoStandby != nil && inst.AutoStandby.Enabled,
			TrackingMode: "conntrack_events_v4_tcp",
			Status:       autostandby.StatusUnsupported,
			Reason:       autostandby.ReasonUnsupportedPlatform,
		}
	} else {
		snapshot, err = s.AutoStandbyController.ResetIdle(ctx, instanceToAutoStandby(*inst))
		if err != nil {
			if errors.Is(err, autostandby.ErrStandbyInProgress) {
				return oapi.ResetAutoStandby409JSONResponse{
					Code:    "instance_in_standby",
					Message: "instance standby is in progress; restore it before connecting",
				}, nil
			}
			log.ErrorContext(ctx, "failed to reset auto-standby idle countdown", "instance_id", inst.Id, "error", err)
			return oapi.ResetAutoStandby500JSONResponse{
				Code:    "internal_error",
				Message: "failed to reset auto-standby idle countdown",
			}, nil
		}

		// Re-check after the reset: a standby that completed between loading
		// the instance and ResetIdle taking the controller lock leaves nothing
		// to reset, so ResetIdle no-ops with a stale snapshot.
		inst, err = s.InstanceManager.GetInstance(ctx, request.Id)
		if err != nil {
			if err == instances.ErrNotFound || err == instances.ErrAmbiguousName {
				return oapi.ResetAutoStandby404JSONResponse{
					Code:    "not_found",
					Message: "instance not found",
				}, nil
			}
			log.ErrorContext(ctx, "failed to reload instance after auto-standby reset", "instance_id", request.Id, "error", err)
			return oapi.ResetAutoStandby500JSONResponse{
				Code:    "internal_error",
				Message: "failed to load instance",
			}, nil
		}
	}

	if inst.State == instances.StateStandby {
		return oapi.ResetAutoStandby409JSONResponse{
			Code:    "instance_in_standby",
			Message: "instance is in standby; restore it before connecting",
		}, nil
	}

	return oapi.ResetAutoStandby200JSONResponse(toOAPIAutoStandbyStatus(snapshot)), nil
}
