package api

import (
	"context"
	"errors"

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
