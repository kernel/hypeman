package dataplane

import (
	"context"
	"strings"

	"github.com/onkernel/cloud-hypervisor-dataplane/cmd/dataplane/config"
	"github.com/onkernel/cloud-hypervisor-dataplane/lib/images"
	"github.com/onkernel/cloud-hypervisor-dataplane/lib/instances"
	"github.com/onkernel/cloud-hypervisor-dataplane/lib/oapi"
	"github.com/onkernel/cloud-hypervisor-dataplane/lib/volumes"
)

// DataplaneService implements the oapi.StrictServerInterface
type DataplaneService struct {
	Config          *config.Config
	ImageManager    images.Manager
	InstanceManager instances.Manager
	VolumeManager   volumes.Manager
}

var _ oapi.StrictServerInterface = (*DataplaneService)(nil)

// NewDataplaneService creates a new dataplane service
func NewDataplaneService(cfg *config.Config) *DataplaneService {
	return &DataplaneService{
		Config:          cfg,
		ImageManager:    images.NewManager(cfg.DataDir),
		InstanceManager: instances.NewManager(cfg.DataDir),
		VolumeManager:   volumes.NewManager(cfg.DataDir),
	}
}

// Health check
func (s *DataplaneService) GetHealth(ctx context.Context, request oapi.GetHealthRequestObject) (oapi.GetHealthResponseObject, error) {
	return oapi.GetHealth200JSONResponse{
		Status: oapi.Ok,
	}, nil
}

// Image operations
func (s *DataplaneService) ListImages(ctx context.Context, request oapi.ListImagesRequestObject) (oapi.ListImagesResponseObject, error) {
	imgs, err := s.ImageManager.ListImages(ctx)
	if err != nil {
		return oapi.ListImages401JSONResponse{
			Code:    "error",
			Message: err.Error(),
		}, nil
	}
	return oapi.ListImages200JSONResponse(imgs), nil
}

func (s *DataplaneService) CreateImage(ctx context.Context, request oapi.CreateImageRequestObject) (oapi.CreateImageResponseObject, error) {
	img, err := s.ImageManager.CreateImage(ctx, *request.Body)
	if err != nil {
		return oapi.CreateImage400JSONResponse{
			Code:    "error",
			Message: err.Error(),
		}, nil
	}
	return oapi.CreateImage201JSONResponse(*img), nil
}

func (s *DataplaneService) GetImage(ctx context.Context, request oapi.GetImageRequestObject) (oapi.GetImageResponseObject, error) {
	img, err := s.ImageManager.GetImage(ctx, request.Id)
	if err != nil {
		return oapi.GetImage404JSONResponse{
			Code:    "not_found",
			Message: err.Error(),
		}, nil
	}
	return oapi.GetImage200JSONResponse(*img), nil
}

func (s *DataplaneService) DeleteImage(ctx context.Context, request oapi.DeleteImageRequestObject) (oapi.DeleteImageResponseObject, error) {
	err := s.ImageManager.DeleteImage(ctx, request.Id)
	if err != nil {
		return oapi.DeleteImage404JSONResponse{
			Code:    "not_found",
			Message: err.Error(),
		}, nil
	}
	return oapi.DeleteImage204Response{}, nil
}

// Instance operations
func (s *DataplaneService) ListInstances(ctx context.Context, request oapi.ListInstancesRequestObject) (oapi.ListInstancesResponseObject, error) {
	insts, err := s.InstanceManager.ListInstances(ctx)
	if err != nil {
		return oapi.ListInstances401JSONResponse{
			Code:    "error",
			Message: err.Error(),
		}, nil
	}
	return oapi.ListInstances200JSONResponse(insts), nil
}

func (s *DataplaneService) CreateInstance(ctx context.Context, request oapi.CreateInstanceRequestObject) (oapi.CreateInstanceResponseObject, error) {
	inst, err := s.InstanceManager.CreateInstance(ctx, *request.Body)
	if err != nil {
		return oapi.CreateInstance400JSONResponse{
			Code:    "error",
			Message: err.Error(),
		}, nil
	}
	return oapi.CreateInstance201JSONResponse(*inst), nil
}

func (s *DataplaneService) GetInstance(ctx context.Context, request oapi.GetInstanceRequestObject) (oapi.GetInstanceResponseObject, error) {
	inst, err := s.InstanceManager.GetInstance(ctx, request.Id)
	if err != nil {
		return oapi.GetInstance404JSONResponse{
			Code:    "not_found",
			Message: err.Error(),
		}, nil
	}
	return oapi.GetInstance200JSONResponse(*inst), nil
}

func (s *DataplaneService) DeleteInstance(ctx context.Context, request oapi.DeleteInstanceRequestObject) (oapi.DeleteInstanceResponseObject, error) {
	err := s.InstanceManager.DeleteInstance(ctx, request.Id)
	if err != nil {
		return oapi.DeleteInstance404JSONResponse{
			Code:    "not_found",
			Message: err.Error(),
		}, nil
	}
	return oapi.DeleteInstance204Response{}, nil
}

func (s *DataplaneService) StandbyInstance(ctx context.Context, request oapi.StandbyInstanceRequestObject) (oapi.StandbyInstanceResponseObject, error) {
	inst, err := s.InstanceManager.StandbyInstance(ctx, request.Id)
	if err != nil {
		return oapi.StandbyInstance404JSONResponse{
			Code:    "error",
			Message: err.Error(),
		}, nil
	}
	return oapi.StandbyInstance200JSONResponse(*inst), nil
}

func (s *DataplaneService) RestoreInstance(ctx context.Context, request oapi.RestoreInstanceRequestObject) (oapi.RestoreInstanceResponseObject, error) {
	inst, err := s.InstanceManager.RestoreInstance(ctx, request.Id)
	if err != nil {
		return oapi.RestoreInstance404JSONResponse{
			Code:    "error",
			Message: err.Error(),
		}, nil
	}
	return oapi.RestoreInstance200JSONResponse(*inst), nil
}

func (s *DataplaneService) GetInstanceLogs(ctx context.Context, request oapi.GetInstanceLogsRequestObject) (oapi.GetInstanceLogsResponseObject, error) {
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

	// Return as plain text for now (SSE would need custom implementation)
	return oapi.GetInstanceLogs200TexteventStreamResponse{
		Body:          strings.NewReader(logs),
		ContentLength: int64(len(logs)),
	}, nil
}

func (s *DataplaneService) AttachVolume(ctx context.Context, request oapi.AttachVolumeRequestObject) (oapi.AttachVolumeResponseObject, error) {
	inst, err := s.InstanceManager.AttachVolume(ctx, request.Id, request.VolumeId, *request.Body)
	if err != nil {
		return oapi.AttachVolume404JSONResponse{
			Code:    "error",
			Message: err.Error(),
		}, nil
	}
	return oapi.AttachVolume200JSONResponse(*inst), nil
}

func (s *DataplaneService) DetachVolume(ctx context.Context, request oapi.DetachVolumeRequestObject) (oapi.DetachVolumeResponseObject, error) {
	inst, err := s.InstanceManager.DetachVolume(ctx, request.Id, request.VolumeId)
	if err != nil {
		return oapi.DetachVolume404JSONResponse{
			Code:    "error",
			Message: err.Error(),
		}, nil
	}
	return oapi.DetachVolume200JSONResponse(*inst), nil
}

// Volume operations
func (s *DataplaneService) ListVolumes(ctx context.Context, request oapi.ListVolumesRequestObject) (oapi.ListVolumesResponseObject, error) {
	vols, err := s.VolumeManager.ListVolumes(ctx)
	if err != nil {
		return oapi.ListVolumes401JSONResponse{
			Code:    "error",
			Message: err.Error(),
		}, nil
	}
	return oapi.ListVolumes200JSONResponse(vols), nil
}

func (s *DataplaneService) CreateVolume(ctx context.Context, request oapi.CreateVolumeRequestObject) (oapi.CreateVolumeResponseObject, error) {
	vol, err := s.VolumeManager.CreateVolume(ctx, *request.Body)
	if err != nil {
		return oapi.CreateVolume400JSONResponse{
			Code:    "error",
			Message: err.Error(),
		}, nil
	}
	return oapi.CreateVolume201JSONResponse(*vol), nil
}

func (s *DataplaneService) GetVolume(ctx context.Context, request oapi.GetVolumeRequestObject) (oapi.GetVolumeResponseObject, error) {
	vol, err := s.VolumeManager.GetVolume(ctx, request.Id)
	if err != nil {
		return oapi.GetVolume404JSONResponse{
			Code:    "not_found",
			Message: err.Error(),
		}, nil
	}
	return oapi.GetVolume200JSONResponse(*vol), nil
}

func (s *DataplaneService) DeleteVolume(ctx context.Context, request oapi.DeleteVolumeRequestObject) (oapi.DeleteVolumeResponseObject, error) {
	err := s.VolumeManager.DeleteVolume(ctx, request.Id)
	if err != nil {
		return oapi.DeleteVolume404JSONResponse{
			Code:    "not_found",
			Message: err.Error(),
		}, nil
	}
	return oapi.DeleteVolume204Response{}, nil
}

