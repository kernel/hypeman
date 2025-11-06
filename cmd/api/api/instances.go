package api

import (
	"context"
	"strings"

	"github.com/onkernel/cloud-hypervisor-dataplane/lib/oapi"
)

// ListInstances lists all instances
func (s *ApiService) ListInstances(ctx context.Context, request oapi.ListInstancesRequestObject) (oapi.ListInstancesResponseObject, error) {
	insts, err := s.InstanceManager.ListInstances(ctx)
	if err != nil {
		return oapi.ListInstances401JSONResponse{
			Code:    "error",
			Message: err.Error(),
		}, nil
	}
	return oapi.ListInstances200JSONResponse(insts), nil
}

// CreateInstance creates and starts a new instance
func (s *ApiService) CreateInstance(ctx context.Context, request oapi.CreateInstanceRequestObject) (oapi.CreateInstanceResponseObject, error) {
	inst, err := s.InstanceManager.CreateInstance(ctx, *request.Body)
	if err != nil {
		return oapi.CreateInstance400JSONResponse{
			Code:    "error",
			Message: err.Error(),
		}, nil
	}
	return oapi.CreateInstance201JSONResponse(*inst), nil
}

// GetInstance gets instance details
func (s *ApiService) GetInstance(ctx context.Context, request oapi.GetInstanceRequestObject) (oapi.GetInstanceResponseObject, error) {
	inst, err := s.InstanceManager.GetInstance(ctx, request.Id)
	if err != nil {
		return oapi.GetInstance404JSONResponse{
			Code:    "not_found",
			Message: err.Error(),
		}, nil
	}
	return oapi.GetInstance200JSONResponse(*inst), nil
}

// DeleteInstance stops and deletes an instance
func (s *ApiService) DeleteInstance(ctx context.Context, request oapi.DeleteInstanceRequestObject) (oapi.DeleteInstanceResponseObject, error) {
	err := s.InstanceManager.DeleteInstance(ctx, request.Id)
	if err != nil {
		return oapi.DeleteInstance404JSONResponse{
			Code:    "not_found",
			Message: err.Error(),
		}, nil
	}
	return oapi.DeleteInstance204Response{}, nil
}

// StandbyInstance puts an instance in standby (pause, snapshot, delete VMM)
func (s *ApiService) StandbyInstance(ctx context.Context, request oapi.StandbyInstanceRequestObject) (oapi.StandbyInstanceResponseObject, error) {
	inst, err := s.InstanceManager.StandbyInstance(ctx, request.Id)
	if err != nil {
		return oapi.StandbyInstance404JSONResponse{
			Code:    "error",
			Message: err.Error(),
		}, nil
	}
	return oapi.StandbyInstance200JSONResponse(*inst), nil
}

// RestoreInstance restores an instance from standby
func (s *ApiService) RestoreInstance(ctx context.Context, request oapi.RestoreInstanceRequestObject) (oapi.RestoreInstanceResponseObject, error) {
	inst, err := s.InstanceManager.RestoreInstance(ctx, request.Id)
	if err != nil {
		return oapi.RestoreInstance404JSONResponse{
			Code:    "error",
			Message: err.Error(),
		}, nil
	}
	return oapi.RestoreInstance200JSONResponse(*inst), nil
}

// GetInstanceLogs streams instance logs
func (s *ApiService) GetInstanceLogs(ctx context.Context, request oapi.GetInstanceLogsRequestObject) (oapi.GetInstanceLogsResponseObject, error) {
	follow := false
	if request.Params.Follow != nil {
		follow = *request.Params.Follow
	}
	tail := 100
	if request.Params.Tail != nil {
		tail = *request.Params.Tail
	}

	logs, err := s.InstanceManager.GetInstanceLogs(ctx, request.Id, follow, tail)
	if err != nil {
		return oapi.GetInstanceLogs404JSONResponse{
			Code:    "not_found",
			Message: err.Error(),
		}, nil
	}

	// Return as SSE stream
	return oapi.GetInstanceLogs200TexteventStreamResponse{
		Body:          strings.NewReader(logs),
		ContentLength: int64(len(logs)),
	}, nil
}

// AttachVolume attaches a volume to an instance
func (s *ApiService) AttachVolume(ctx context.Context, request oapi.AttachVolumeRequestObject) (oapi.AttachVolumeResponseObject, error) {
	inst, err := s.InstanceManager.AttachVolume(ctx, request.Id, request.VolumeId, *request.Body)
	if err != nil {
		return oapi.AttachVolume404JSONResponse{
			Code:    "error",
			Message: err.Error(),
		}, nil
	}
	return oapi.AttachVolume200JSONResponse(*inst), nil
}

// DetachVolume detaches a volume from an instance
func (s *ApiService) DetachVolume(ctx context.Context, request oapi.DetachVolumeRequestObject) (oapi.DetachVolumeResponseObject, error) {
	inst, err := s.InstanceManager.DetachVolume(ctx, request.Id, request.VolumeId)
	if err != nil {
		return oapi.DetachVolume404JSONResponse{
			Code:    "error",
			Message: err.Error(),
		}, nil
	}
	return oapi.DetachVolume200JSONResponse(*inst), nil
}

