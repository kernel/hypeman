package api

import (
	"context"

	"github.com/kernel/hypeman/lib/autostandby"
	"github.com/kernel/hypeman/lib/instances"
	"github.com/kernel/hypeman/lib/logger"
	"github.com/kernel/hypeman/lib/oapi"
	"github.com/samber/lo"
)

func (s *ApiService) GetAutoStandbyStatus(ctx context.Context, request oapi.GetAutoStandbyStatusRequestObject) (oapi.GetAutoStandbyStatusResponseObject, error) {
	log := logger.FromContext(ctx)

	inst, err := s.InstanceManager.GetInstance(ctx, request.Id)
	if err != nil {
		if err == instances.ErrNotFound || err == instances.ErrAmbiguousName {
			return oapi.GetAutoStandbyStatus404JSONResponse{
				Code:    "not_found",
				Message: "instance not found",
			}, nil
		}
		log.ErrorContext(ctx, "failed to resolve instance for auto-standby status", "instance_id", request.Id, "error", err)
		return oapi.GetAutoStandbyStatus500JSONResponse{
			Code:    "internal_error",
			Message: "failed to load instance",
		}, nil
	}

	snapshot := autostandby.StatusSnapshot{
		Supported:    false,
		Configured:   inst.AutoStandby != nil,
		Enabled:      inst.AutoStandby != nil && inst.AutoStandby.Enabled,
		TrackingMode: "conntrack_events_v4_tcp",
		Status:       autostandby.StatusUnsupported,
		Reason:       autostandby.ReasonUnsupportedPlatform,
	}
	if s.AutoStandbyController != nil {
		snapshot = s.AutoStandbyController.Describe(instanceToAutoStandby(*inst))
	}

	return oapi.GetAutoStandbyStatus200JSONResponse(toOAPIAutoStandbyStatus(snapshot)), nil
}

func instanceToAutoStandby(inst instances.Instance) autostandby.Instance {
	return autostandby.Instance{
		ID:             inst.Id,
		Name:           inst.Name,
		State:          string(inst.State),
		NetworkEnabled: inst.NetworkEnabled,
		IP:             inst.IP,
		HasVGPU:        inst.GPUProfile != "" || inst.GPUMdevUUID != "",
		AutoStandby:    inst.AutoStandby,
	}
}

func toOAPIAutoStandbyStatus(status autostandby.StatusSnapshot) oapi.AutoStandbyStatus {
	out := oapi.AutoStandbyStatus{
		ActiveInboundConnections: status.ActiveInboundCount,
		Configured:               status.Configured,
		Eligible:                 status.Eligible,
		Enabled:                  status.Enabled,
		Reason:                   oapi.AutoStandbyStatusReason(status.Reason),
		Status:                   oapi.AutoStandbyStatusStatus(status.Status),
		Supported:                status.Supported,
		TrackingMode:             status.TrackingMode,
	}
	if status.IdleTimeout != "" {
		out.IdleTimeout = lo.ToPtr(status.IdleTimeout)
	}
	out.IdleSince = status.IdleSince
	out.LastInboundActivityAt = status.LastInboundActivityAt
	out.NextStandbyAt = status.NextStandbyAt
	if status.CountdownRemaining != nil {
		out.CountdownRemaining = lo.ToPtr(status.CountdownRemaining.String())
	}
	return out
}

