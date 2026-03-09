package api

import (
	"context"
	"errors"
	"time"

	"github.com/kernel/hypeman/lib/hypervisor"
	"github.com/kernel/hypeman/lib/instances"
	"github.com/kernel/hypeman/lib/logger"
	mw "github.com/kernel/hypeman/lib/middleware"
	"github.com/kernel/hypeman/lib/network"
	"github.com/kernel/hypeman/lib/oapi"
	"github.com/samber/lo"
)

// CreateInstanceSnapshot creates a snapshot for the resolved instance.
func (s *ApiService) CreateInstanceSnapshot(ctx context.Context, request oapi.CreateInstanceSnapshotRequestObject) (oapi.CreateInstanceSnapshotResponseObject, error) {
	inst := mw.GetResolvedInstance[instances.Instance](ctx)
	if inst == nil {
		return oapi.CreateInstanceSnapshot500JSONResponse{Code: "internal_error", Message: "resource not resolved"}, nil
	}
	if request.Body == nil {
		return oapi.CreateInstanceSnapshot400JSONResponse{Code: "invalid_request", Message: "request body is required"}, nil
	}

	var name string
	if request.Body.Name != nil {
		name = *request.Body.Name
	}

	result, err := s.InstanceManager.CreateSnapshot(ctx, inst.Id, instances.CreateSnapshotRequest{
		Kind:     instances.SnapshotKind(request.Body.Kind),
		Name:     name,
		Metadata: toMapMetadata(request.Body.Metadata),
	})
	if err != nil {
		log := logger.FromContext(ctx)
		switch {
		case errors.Is(err, instances.ErrNotFound):
			return oapi.CreateInstanceSnapshot404JSONResponse{Code: "not_found", Message: "instance not found"}, nil
		case errors.Is(err, instances.ErrInvalidRequest):
			return oapi.CreateInstanceSnapshot400JSONResponse{Code: "invalid_request", Message: err.Error()}, nil
		case errors.Is(err, instances.ErrInvalidState), errors.Is(err, instances.ErrAlreadyExists):
			return oapi.CreateInstanceSnapshot409JSONResponse{Code: "conflict", Message: err.Error()}, nil
		case errors.Is(err, instances.ErrNotSupported):
			return oapi.CreateInstanceSnapshot501JSONResponse{Code: "not_supported", Message: err.Error()}, nil
		default:
			log.ErrorContext(ctx, "failed to create snapshot", "error", err)
			return oapi.CreateInstanceSnapshot500JSONResponse{Code: "internal_error", Message: "failed to create snapshot"}, nil
		}
	}

	return oapi.CreateInstanceSnapshot201JSONResponse(snapshotToOAPI(*result)), nil
}

// RestoreInstanceSnapshot restores an instance from a snapshot in-place.
func (s *ApiService) RestoreInstanceSnapshot(ctx context.Context, request oapi.RestoreInstanceSnapshotRequestObject) (oapi.RestoreInstanceSnapshotResponseObject, error) {
	inst := mw.GetResolvedInstance[instances.Instance](ctx)
	if inst == nil {
		return oapi.RestoreInstanceSnapshot500JSONResponse{Code: "internal_error", Message: "resource not resolved"}, nil
	}
	if request.Body == nil {
		return oapi.RestoreInstanceSnapshot400JSONResponse{Code: "invalid_request", Message: "request body is required"}, nil
	}

	domainReq := instances.RestoreSnapshotRequest{}
	if request.Body.TargetState != nil {
		domainReq.TargetState = instances.State(*request.Body.TargetState)
	}
	if request.Body.TargetHypervisor != nil {
		domainReq.TargetHypervisor = hypervisor.Type(*request.Body.TargetHypervisor)
	}

	result, err := s.InstanceManager.RestoreSnapshot(ctx, inst.Id, request.SnapshotId, domainReq)
	if err != nil {
		log := logger.FromContext(ctx)
		switch {
		case errors.Is(err, instances.ErrNotFound), errors.Is(err, instances.ErrSnapshotNotFound):
			return oapi.RestoreInstanceSnapshot404JSONResponse{Code: "not_found", Message: "instance or snapshot not found"}, nil
		case errors.Is(err, instances.ErrInvalidRequest):
			return oapi.RestoreInstanceSnapshot400JSONResponse{Code: "invalid_request", Message: err.Error()}, nil
		case errors.Is(err, instances.ErrInvalidState):
			return oapi.RestoreInstanceSnapshot409JSONResponse{Code: "invalid_state", Message: err.Error()}, nil
		case errors.Is(err, instances.ErrNotSupported):
			return oapi.RestoreInstanceSnapshot501JSONResponse{Code: "not_supported", Message: err.Error()}, nil
		default:
			log.ErrorContext(ctx, "failed to restore snapshot", "error", err)
			return oapi.RestoreInstanceSnapshot500JSONResponse{Code: "internal_error", Message: "failed to restore snapshot"}, nil
		}
	}

	return oapi.RestoreInstanceSnapshot200JSONResponse(instanceToOAPI(*result)), nil
}

// ListSnapshots lists centrally managed snapshots with optional filters.
func (s *ApiService) ListSnapshots(ctx context.Context, request oapi.ListSnapshotsRequestObject) (oapi.ListSnapshotsResponseObject, error) {
	filter := &instances.ListSnapshotsFilter{}
	if request.Params.SourceInstanceId != nil {
		filter.SourceInstanceID = request.Params.SourceInstanceId
	}
	if request.Params.Kind != nil {
		kind := instances.SnapshotKind(*request.Params.Kind)
		filter.Kind = &kind
	}
	if request.Params.Name != nil {
		filter.Name = request.Params.Name
	}
	if request.Params.Metadata != nil {
		filter.Metadata = toMapMetadata(request.Params.Metadata)
	}
	if filter.SourceInstanceID == nil && filter.Kind == nil && filter.Name == nil && len(filter.Metadata) == 0 {
		filter = nil
	}

	snaps, err := s.InstanceManager.ListSnapshots(ctx, filter)
	if err != nil {
		log := logger.FromContext(ctx)
		log.ErrorContext(ctx, "failed to list snapshots", "error", err)
		return oapi.ListSnapshots500JSONResponse{Code: "internal_error", Message: "failed to list snapshots"}, nil
	}

	resp := make([]oapi.Snapshot, len(snaps))
	for i := range snaps {
		resp[i] = snapshotToOAPI(snaps[i])
	}
	return oapi.ListSnapshots200JSONResponse(resp), nil
}

// GetSnapshot returns details for a snapshot.
func (s *ApiService) GetSnapshot(ctx context.Context, request oapi.GetSnapshotRequestObject) (oapi.GetSnapshotResponseObject, error) {
	snap, err := s.InstanceManager.GetSnapshot(ctx, request.SnapshotId)
	if err != nil {
		log := logger.FromContext(ctx)
		switch {
		case errors.Is(err, instances.ErrSnapshotNotFound):
			return oapi.GetSnapshot404JSONResponse{Code: "not_found", Message: "snapshot not found"}, nil
		default:
			log.ErrorContext(ctx, "failed to get snapshot", "error", err)
			return oapi.GetSnapshot500JSONResponse{Code: "internal_error", Message: "failed to get snapshot"}, nil
		}
	}
	return oapi.GetSnapshot200JSONResponse(snapshotToOAPI(*snap)), nil
}

// DeleteSnapshot deletes a snapshot.
func (s *ApiService) DeleteSnapshot(ctx context.Context, request oapi.DeleteSnapshotRequestObject) (oapi.DeleteSnapshotResponseObject, error) {
	err := s.InstanceManager.DeleteSnapshot(ctx, request.SnapshotId)
	if err != nil {
		log := logger.FromContext(ctx)
		switch {
		case errors.Is(err, instances.ErrSnapshotNotFound):
			return oapi.DeleteSnapshot404JSONResponse{Code: "not_found", Message: "snapshot not found"}, nil
		default:
			log.ErrorContext(ctx, "failed to delete snapshot", "error", err)
			return oapi.DeleteSnapshot500JSONResponse{Code: "internal_error", Message: "failed to delete snapshot"}, nil
		}
	}
	return oapi.DeleteSnapshot204Response{}, nil
}

// ForkSnapshot creates a new instance from a snapshot.
func (s *ApiService) ForkSnapshot(ctx context.Context, request oapi.ForkSnapshotRequestObject) (oapi.ForkSnapshotResponseObject, error) {
	if request.Body == nil {
		return oapi.ForkSnapshot400JSONResponse{Code: "invalid_request", Message: "request body is required"}, nil
	}

	domainReq := instances.ForkSnapshotRequest{Name: request.Body.Name}
	if request.Body.TargetState != nil {
		domainReq.TargetState = instances.State(*request.Body.TargetState)
	}
	if request.Body.TargetHypervisor != nil {
		domainReq.TargetHypervisor = hypervisor.Type(*request.Body.TargetHypervisor)
	}

	result, err := s.InstanceManager.ForkSnapshot(ctx, request.SnapshotId, domainReq)
	if err != nil {
		log := logger.FromContext(ctx)
		switch {
		case errors.Is(err, instances.ErrSnapshotNotFound):
			return oapi.ForkSnapshot404JSONResponse{Code: "not_found", Message: "snapshot not found"}, nil
		case errors.Is(err, instances.ErrInvalidRequest):
			return oapi.ForkSnapshot400JSONResponse{Code: "invalid_request", Message: err.Error()}, nil
		case errors.Is(err, instances.ErrInvalidState), errors.Is(err, instances.ErrAlreadyExists), errors.Is(err, network.ErrNameExists):
			return oapi.ForkSnapshot409JSONResponse{Code: "conflict", Message: err.Error()}, nil
		case errors.Is(err, instances.ErrNotSupported):
			return oapi.ForkSnapshot501JSONResponse{Code: "not_supported", Message: err.Error()}, nil
		default:
			log.ErrorContext(ctx, "failed to fork snapshot", "error", err)
			return oapi.ForkSnapshot500JSONResponse{Code: "internal_error", Message: "failed to fork snapshot"}, nil
		}
	}

	return oapi.ForkSnapshot201JSONResponse(instanceToOAPI(*result)), nil
}

// GetInstanceSnapshotSchedule gets a snapshot schedule for the resolved instance.
func (s *ApiService) GetInstanceSnapshotSchedule(ctx context.Context, request oapi.GetInstanceSnapshotScheduleRequestObject) (oapi.GetInstanceSnapshotScheduleResponseObject, error) {
	inst := mw.GetResolvedInstance[instances.Instance](ctx)
	if inst == nil {
		return oapi.GetInstanceSnapshotSchedule500JSONResponse{Code: "internal_error", Message: "resource not resolved"}, nil
	}

	scheduleManager, ok := s.InstanceManager.(instances.SnapshotScheduleManager)
	if !ok {
		return oapi.GetInstanceSnapshotSchedule500JSONResponse{Code: "internal_error", Message: "snapshot scheduling is not available"}, nil
	}

	schedule, err := scheduleManager.GetSnapshotSchedule(ctx, inst.Id)
	if err != nil {
		log := logger.FromContext(ctx)
		switch {
		case errors.Is(err, instances.ErrSnapshotScheduleNotFound):
			return oapi.GetInstanceSnapshotSchedule404JSONResponse{Code: "not_found", Message: "snapshot schedule not found"}, nil
		case errors.Is(err, instances.ErrNotFound):
			return oapi.GetInstanceSnapshotSchedule404JSONResponse{Code: "not_found", Message: "instance not found"}, nil
		default:
			log.ErrorContext(ctx, "failed to get snapshot schedule", "error", err)
			return oapi.GetInstanceSnapshotSchedule500JSONResponse{Code: "internal_error", Message: "failed to get snapshot schedule"}, nil
		}
	}

	return oapi.GetInstanceSnapshotSchedule200JSONResponse(snapshotScheduleToOAPI(*schedule)), nil
}

// SetInstanceSnapshotSchedule creates or updates a snapshot schedule for the resolved instance.
func (s *ApiService) SetInstanceSnapshotSchedule(ctx context.Context, request oapi.SetInstanceSnapshotScheduleRequestObject) (oapi.SetInstanceSnapshotScheduleResponseObject, error) {
	inst := mw.GetResolvedInstance[instances.Instance](ctx)
	if inst == nil {
		return oapi.SetInstanceSnapshotSchedule500JSONResponse{Code: "internal_error", Message: "resource not resolved"}, nil
	}
	if request.Body == nil {
		return oapi.SetInstanceSnapshotSchedule400JSONResponse{Code: "invalid_request", Message: "request body is required"}, nil
	}

	scheduleManager, ok := s.InstanceManager.(instances.SnapshotScheduleManager)
	if !ok {
		return oapi.SetInstanceSnapshotSchedule500JSONResponse{Code: "internal_error", Message: "snapshot scheduling is not available"}, nil
	}

	interval, err := time.ParseDuration(request.Body.Interval)
	if err != nil {
		return oapi.SetInstanceSnapshotSchedule400JSONResponse{Code: "invalid_request", Message: "interval must be a valid duration"}, nil
	}

	req := instances.SetSnapshotScheduleRequest{
		Interval: interval,
		Metadata: toMapMetadata(request.Body.Metadata),
		Retention: instances.SnapshotScheduleRetention{
			MaxCount: 0,
			MaxAge:   0,
		},
	}
	if request.Body.NamePrefix != nil {
		req.NamePrefix = *request.Body.NamePrefix
	}
	if request.Body.Retention.MaxCount != nil {
		req.Retention.MaxCount = *request.Body.Retention.MaxCount
	}
	if request.Body.Retention.MaxAge != nil {
		maxAge, parseErr := time.ParseDuration(*request.Body.Retention.MaxAge)
		if parseErr != nil {
			return oapi.SetInstanceSnapshotSchedule400JSONResponse{Code: "invalid_request", Message: "retention.max_age must be a valid duration"}, nil
		}
		req.Retention.MaxAge = maxAge
	}

	schedule, err := scheduleManager.SetSnapshotSchedule(ctx, inst.Id, req)
	if err != nil {
		log := logger.FromContext(ctx)
		switch {
		case errors.Is(err, instances.ErrInvalidRequest):
			return oapi.SetInstanceSnapshotSchedule400JSONResponse{Code: "invalid_request", Message: err.Error()}, nil
		default:
			log.ErrorContext(ctx, "failed to set snapshot schedule", "error", err)
			return oapi.SetInstanceSnapshotSchedule500JSONResponse{Code: "internal_error", Message: "failed to set snapshot schedule"}, nil
		}
	}

	return oapi.SetInstanceSnapshotSchedule200JSONResponse(snapshotScheduleToOAPI(*schedule)), nil
}

// DeleteInstanceSnapshotSchedule deletes a snapshot schedule for the resolved instance.
func (s *ApiService) DeleteInstanceSnapshotSchedule(ctx context.Context, request oapi.DeleteInstanceSnapshotScheduleRequestObject) (oapi.DeleteInstanceSnapshotScheduleResponseObject, error) {
	inst := mw.GetResolvedInstance[instances.Instance](ctx)
	if inst == nil {
		return oapi.DeleteInstanceSnapshotSchedule500JSONResponse{Code: "internal_error", Message: "resource not resolved"}, nil
	}

	scheduleManager, ok := s.InstanceManager.(instances.SnapshotScheduleManager)
	if !ok {
		return oapi.DeleteInstanceSnapshotSchedule500JSONResponse{Code: "internal_error", Message: "snapshot scheduling is not available"}, nil
	}

	err := scheduleManager.DeleteSnapshotSchedule(ctx, inst.Id)
	if err != nil {
		log := logger.FromContext(ctx)
		switch {
		case errors.Is(err, instances.ErrSnapshotScheduleNotFound):
			return oapi.DeleteInstanceSnapshotSchedule404JSONResponse{Code: "not_found", Message: "snapshot schedule not found"}, nil
		case errors.Is(err, instances.ErrNotFound):
			return oapi.DeleteInstanceSnapshotSchedule404JSONResponse{Code: "not_found", Message: "instance not found"}, nil
		default:
			log.ErrorContext(ctx, "failed to delete snapshot schedule", "error", err)
			return oapi.DeleteInstanceSnapshotSchedule500JSONResponse{Code: "internal_error", Message: "failed to delete snapshot schedule"}, nil
		}
	}

	return oapi.DeleteInstanceSnapshotSchedule204Response{}, nil
}

func snapshotToOAPI(snapshot instances.Snapshot) oapi.Snapshot {
	kind := oapi.SnapshotKind(snapshot.Kind)
	sourceHypervisor := oapi.SnapshotSourceHypervisor(snapshot.SourceHypervisor)
	out := oapi.Snapshot{
		Id:                 snapshot.Id,
		Kind:               kind,
		Metadata:           toOAPIMetadata(snapshot.Metadata),
		SourceInstanceId:   snapshot.SourceInstanceID,
		SourceInstanceName: snapshot.SourceName,
		SourceHypervisor:   sourceHypervisor,
		CreatedAt:          snapshot.CreatedAt,
		SizeBytes:          snapshot.SizeBytes,
		Name:               lo.ToPtr(snapshot.Name),
	}
	if snapshot.Name == "" {
		out.Name = nil
	}
	return out
}

func snapshotScheduleToOAPI(schedule instances.SnapshotSchedule) oapi.SnapshotSchedule {
	retention := oapi.SnapshotScheduleRetention{}
	if schedule.Retention.MaxCount > 0 {
		retention.MaxCount = lo.ToPtr(schedule.Retention.MaxCount)
	}
	if schedule.Retention.MaxAge > 0 {
		maxAge := schedule.Retention.MaxAge.String()
		retention.MaxAge = &maxAge
	}

	out := oapi.SnapshotSchedule{
		InstanceId: schedule.InstanceID,
		Interval:   schedule.Interval.String(),
		Metadata:   toOAPIMetadata(schedule.Metadata),
		Retention:  retention,
		NextRunAt:  schedule.NextRunAt,
		CreatedAt:  schedule.CreatedAt,
		UpdatedAt:  schedule.UpdatedAt,
	}
	if schedule.NamePrefix != "" {
		out.NamePrefix = lo.ToPtr(schedule.NamePrefix)
	}
	if schedule.LastRunAt != nil {
		out.LastRunAt = schedule.LastRunAt
	}
	if schedule.LastSnapshotID != nil {
		out.LastSnapshotId = schedule.LastSnapshotID
	}
	if schedule.LastError != nil {
		out.LastError = schedule.LastError
	}
	return out
}
