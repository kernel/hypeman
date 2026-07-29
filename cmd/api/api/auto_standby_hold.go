package api

import (
	"context"
	"errors"

	"github.com/kernel/hypeman/lib/autostandby"
	"github.com/kernel/hypeman/lib/instances"
	"github.com/kernel/hypeman/lib/logger"
	mw "github.com/kernel/hypeman/lib/middleware"
	"github.com/kernel/hypeman/lib/oapi"
)

func (s *ApiService) HoldAutoStandby(ctx context.Context, request oapi.HoldAutoStandbyRequestObject) (oapi.HoldAutoStandbyResponseObject, error) {
	log := logger.FromContext(ctx)

	inst := mw.GetResolvedInstance[instances.Instance](ctx)
	if inst == nil {
		return oapi.HoldAutoStandby500JSONResponse{
			Code:    "internal_error",
			Message: "resource not resolved",
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
		var err error
		snapshot, err = s.AutoStandbyController.HoldStandby(ctx, instanceToAutoStandby(*inst))
		if err != nil {
			if errors.Is(err, autostandby.ErrStandbyInProgress) {
				return oapi.HoldAutoStandby409JSONResponse{
					Code:    "instance_in_standby",
					Message: "instance standby is in progress; restore it before connecting",
				}, nil
			}
			log.ErrorContext(ctx, "failed to place auto-standby hold", "instance_id", inst.Id, "error", err)
			return oapi.HoldAutoStandby500JSONResponse{
				Code:    "internal_error",
				Message: "failed to place auto-standby hold",
			}, nil
		}

		// The instance can complete a standby between resolution and the hold
		// taking effect; re-check so a 200 never describes a standby instance.
		// Reloading by resolved ID skips the name/prefix resolution path.
		inst, err = s.InstanceManager.GetInstance(ctx, inst.Id)
		if err != nil {
			if err == instances.ErrNotFound || err == instances.ErrAmbiguousName {
				return oapi.HoldAutoStandby404JSONResponse{
					Code:    "not_found",
					Message: "instance not found",
				}, nil
			}
			log.ErrorContext(ctx, "failed to reload instance after auto-standby hold", "instance_id", request.Id, "error", err)
			return oapi.HoldAutoStandby500JSONResponse{
				Code:    "internal_error",
				Message: "failed to load instance",
			}, nil
		}
	}

	if inst.State == instances.StateStandby {
		return oapi.HoldAutoStandby409JSONResponse{
			Code:    "instance_in_standby",
			Message: "instance is in standby; restore it before connecting",
		}, nil
	}

	return oapi.HoldAutoStandby200JSONResponse(toOAPIAutoStandbyStatus(snapshot)), nil
}
