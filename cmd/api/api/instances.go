package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/c2h5oh/datasize"
	"github.com/onkernel/hypeman/lib/instances"
	"github.com/onkernel/hypeman/lib/logger"
	"github.com/onkernel/hypeman/lib/network"
	"github.com/onkernel/hypeman/lib/oapi"
	"github.com/samber/lo"
)

// ListInstances lists all instances
func (s *ApiService) ListInstances(ctx context.Context, request oapi.ListInstancesRequestObject) (oapi.ListInstancesResponseObject, error) {
	log := logger.FromContext(ctx)

	domainInsts, err := s.InstanceManager.ListInstances(ctx)
	if err != nil {
		log.ErrorContext(ctx, "failed to list instances", "error", err)
		return oapi.ListInstances500JSONResponse{
			Code:    "internal_error",
			Message: "failed to list instances",
		}, nil
	}

	oapiInsts := make([]oapi.Instance, len(domainInsts))
	for i, inst := range domainInsts {
		oapiInsts[i] = instanceToOAPI(inst)
	}

	return oapi.ListInstances200JSONResponse(oapiInsts), nil
}

// CreateInstance creates and starts a new instance
func (s *ApiService) CreateInstance(ctx context.Context, request oapi.CreateInstanceRequestObject) (oapi.CreateInstanceResponseObject, error) {
	log := logger.FromContext(ctx)

	// Parse size (default: 1GB)
	size := int64(0)
	if request.Body.Size != nil && *request.Body.Size != "" {
		var sizeBytes datasize.ByteSize
		if err := sizeBytes.UnmarshalText([]byte(*request.Body.Size)); err != nil {
			return oapi.CreateInstance400JSONResponse{
				Code:    "invalid_size",
				Message: fmt.Sprintf("invalid size format: %v", err),
			}, nil
		}
		size = int64(sizeBytes)
	}

	// Parse hotplug_size (default: 3GB)
	hotplugSize := int64(0)
	if request.Body.HotplugSize != nil && *request.Body.HotplugSize != "" {
		var hotplugBytes datasize.ByteSize
		if err := hotplugBytes.UnmarshalText([]byte(*request.Body.HotplugSize)); err != nil {
			return oapi.CreateInstance400JSONResponse{
				Code:    "invalid_hotplug_size",
				Message: fmt.Sprintf("invalid hotplug_size format: %v", err),
			}, nil
		}
		hotplugSize = int64(hotplugBytes)
	}

	// Parse overlay_size (default: 10GB)
	overlaySize := int64(0)
	if request.Body.OverlaySize != nil && *request.Body.OverlaySize != "" {
		var overlayBytes datasize.ByteSize
		if err := overlayBytes.UnmarshalText([]byte(*request.Body.OverlaySize)); err != nil {
			return oapi.CreateInstance400JSONResponse{
				Code:    "invalid_overlay_size",
				Message: fmt.Sprintf("invalid overlay_size format: %v", err),
			}, nil
		}
		overlaySize = int64(overlayBytes)
	}

	vcpus := 2
	if request.Body.Vcpus != nil {
		vcpus = *request.Body.Vcpus
	}

	env := make(map[string]string)
	if request.Body.Env != nil {
		env = *request.Body.Env
	}

	// Parse network enabled (default: true)
	networkEnabled := true
	if request.Body.Network != nil && request.Body.Network.Enabled != nil {
		networkEnabled = *request.Body.Network.Enabled
	}

	// Parse volumes
	var volumes []instances.VolumeAttachment
	if request.Body.Volumes != nil {
		volumes = make([]instances.VolumeAttachment, len(*request.Body.Volumes))
		for i, vol := range *request.Body.Volumes {
			readonly := false
			if vol.Readonly != nil {
				readonly = *vol.Readonly
			}
			overlay := false
			if vol.Overlay != nil {
				overlay = *vol.Overlay
			}
			var overlaySize int64
			if vol.OverlaySize != nil && *vol.OverlaySize != "" {
				var overlaySizeBytes datasize.ByteSize
				if err := overlaySizeBytes.UnmarshalText([]byte(*vol.OverlaySize)); err != nil {
					return oapi.CreateInstance400JSONResponse{
						Code:    "invalid_overlay_size",
						Message: fmt.Sprintf("invalid overlay_size for volume %s: %v", vol.VolumeId, err),
					}, nil
				}
				overlaySize = int64(overlaySizeBytes)
			}
			volumes[i] = instances.VolumeAttachment{
				VolumeID:    vol.VolumeId,
				MountPath:   vol.MountPath,
				Readonly:    readonly,
				Overlay:     overlay,
				OverlaySize: overlaySize,
			}
		}
	}

	domainReq := instances.CreateInstanceRequest{
		Name:           request.Body.Name,
		Image:          request.Body.Image,
		Size:           size,
		HotplugSize:    hotplugSize,
		OverlaySize:    overlaySize,
		Vcpus:          vcpus,
		Env:            env,
		NetworkEnabled: networkEnabled,
		Volumes:        volumes,
	}

	inst, err := s.InstanceManager.CreateInstance(ctx, domainReq)
	if err != nil {
		switch {
		case errors.Is(err, instances.ErrImageNotReady):
			return oapi.CreateInstance400JSONResponse{
				Code:    "image_not_ready",
				Message: err.Error(),
			}, nil
		case errors.Is(err, instances.ErrAlreadyExists):
			return oapi.CreateInstance400JSONResponse{
				Code:    "already_exists",
				Message: "instance already exists",
			}, nil
		case errors.Is(err, network.ErrNameExists):
			return oapi.CreateInstance400JSONResponse{
				Code:    "name_conflict",
				Message: err.Error(),
			}, nil
		default:
			log.ErrorContext(ctx, "failed to create instance", "error", err, "image", request.Body.Image)
			return oapi.CreateInstance500JSONResponse{
				Code:    "internal_error",
				Message: "failed to create instance",
			}, nil
		}
	}
	return oapi.CreateInstance201JSONResponse(instanceToOAPI(*inst)), nil
}

// GetInstance gets instance details
// The id parameter can be an instance ID, name, or ID prefix
func (s *ApiService) GetInstance(ctx context.Context, request oapi.GetInstanceRequestObject) (oapi.GetInstanceResponseObject, error) {
	log := logger.FromContext(ctx)

	inst, err := s.InstanceManager.GetInstance(ctx, request.Id)
	if err != nil {
		switch {
		case errors.Is(err, instances.ErrNotFound):
			return oapi.GetInstance404JSONResponse{
				Code:    "not_found",
				Message: "instance not found",
			}, nil
		case errors.Is(err, instances.ErrAmbiguousName):
			return oapi.GetInstance404JSONResponse{
				Code:    "ambiguous",
				Message: "multiple instances match, use full instance ID",
			}, nil
		default:
			log.ErrorContext(ctx, "failed to get instance", "error", err, "id", request.Id)
			return oapi.GetInstance500JSONResponse{
				Code:    "internal_error",
				Message: "failed to get instance",
			}, nil
		}
	}
	return oapi.GetInstance200JSONResponse(instanceToOAPI(*inst)), nil
}

// DeleteInstance stops and deletes an instance
// The id parameter can be an instance ID, name, or ID prefix
func (s *ApiService) DeleteInstance(ctx context.Context, request oapi.DeleteInstanceRequestObject) (oapi.DeleteInstanceResponseObject, error) {
	log := logger.FromContext(ctx)

	// Resolve to get the actual instance ID
	inst, err := s.InstanceManager.GetInstance(ctx, request.Id)
	if err != nil {
		switch {
		case errors.Is(err, instances.ErrNotFound):
			return oapi.DeleteInstance404JSONResponse{
				Code:    "not_found",
				Message: "instance not found",
			}, nil
		case errors.Is(err, instances.ErrAmbiguousName):
			return oapi.DeleteInstance404JSONResponse{
				Code:    "ambiguous",
				Message: "multiple instances match, use full instance ID",
			}, nil
		default:
			log.ErrorContext(ctx, "failed to get instance", "error", err, "id", request.Id)
			return oapi.DeleteInstance500JSONResponse{
				Code:    "internal_error",
				Message: "failed to get instance",
			}, nil
		}
	}

	err = s.InstanceManager.DeleteInstance(ctx, inst.Id)
	if err != nil {
		log.ErrorContext(ctx, "failed to delete instance", "error", err, "id", inst.Id)
		return oapi.DeleteInstance500JSONResponse{
			Code:    "internal_error",
			Message: "failed to delete instance",
		}, nil
	}
	return oapi.DeleteInstance204Response{}, nil
}

// StandbyInstance puts an instance in standby (pause, snapshot, delete VMM)
// The id parameter can be an instance ID, name, or ID prefix
func (s *ApiService) StandbyInstance(ctx context.Context, request oapi.StandbyInstanceRequestObject) (oapi.StandbyInstanceResponseObject, error) {
	log := logger.FromContext(ctx)

	// Resolve to get the actual instance ID
	resolved, err := s.InstanceManager.GetInstance(ctx, request.Id)
	if err != nil {
		switch {
		case errors.Is(err, instances.ErrNotFound):
			return oapi.StandbyInstance404JSONResponse{
				Code:    "not_found",
				Message: "instance not found",
			}, nil
		case errors.Is(err, instances.ErrAmbiguousName):
			return oapi.StandbyInstance404JSONResponse{
				Code:    "ambiguous",
				Message: "multiple instances match, use full instance ID",
			}, nil
		default:
			log.ErrorContext(ctx, "failed to get instance", "error", err, "id", request.Id)
			return oapi.StandbyInstance500JSONResponse{
				Code:    "internal_error",
				Message: "failed to get instance",
			}, nil
		}
	}

	inst, err := s.InstanceManager.StandbyInstance(ctx, resolved.Id)
	if err != nil {
		switch {
		case errors.Is(err, instances.ErrInvalidState):
			return oapi.StandbyInstance409JSONResponse{
				Code:    "invalid_state",
				Message: err.Error(),
			}, nil
		default:
			log.ErrorContext(ctx, "failed to standby instance", "error", err, "id", resolved.Id)
			return oapi.StandbyInstance500JSONResponse{
				Code:    "internal_error",
				Message: "failed to standby instance",
			}, nil
		}
	}
	return oapi.StandbyInstance200JSONResponse(instanceToOAPI(*inst)), nil
}

// RestoreInstance restores an instance from standby
// The id parameter can be an instance ID, name, or ID prefix
func (s *ApiService) RestoreInstance(ctx context.Context, request oapi.RestoreInstanceRequestObject) (oapi.RestoreInstanceResponseObject, error) {
	log := logger.FromContext(ctx)

	// Resolve to get the actual instance ID
	resolved, err := s.InstanceManager.GetInstance(ctx, request.Id)
	if err != nil {
		switch {
		case errors.Is(err, instances.ErrNotFound):
			return oapi.RestoreInstance404JSONResponse{
				Code:    "not_found",
				Message: "instance not found",
			}, nil
		case errors.Is(err, instances.ErrAmbiguousName):
			return oapi.RestoreInstance404JSONResponse{
				Code:    "ambiguous",
				Message: "multiple instances match, use full instance ID",
			}, nil
		default:
			log.ErrorContext(ctx, "failed to get instance", "error", err, "id", request.Id)
			return oapi.RestoreInstance500JSONResponse{
				Code:    "internal_error",
				Message: "failed to get instance",
			}, nil
		}
	}

	inst, err := s.InstanceManager.RestoreInstance(ctx, resolved.Id)
	if err != nil {
		switch {
		case errors.Is(err, instances.ErrInvalidState):
			return oapi.RestoreInstance409JSONResponse{
				Code:    "invalid_state",
				Message: err.Error(),
			}, nil
		default:
			log.ErrorContext(ctx, "failed to restore instance", "error", err, "id", resolved.Id)
			return oapi.RestoreInstance500JSONResponse{
				Code:    "internal_error",
				Message: "failed to restore instance",
			}, nil
		}
	}
	return oapi.RestoreInstance200JSONResponse(instanceToOAPI(*inst)), nil
}

// StopInstance gracefully stops a running instance
// The id parameter can be an instance ID, name, or ID prefix
func (s *ApiService) StopInstance(ctx context.Context, request oapi.StopInstanceRequestObject) (oapi.StopInstanceResponseObject, error) {
	log := logger.FromContext(ctx)

	// Resolve to get the actual instance ID
	resolved, err := s.InstanceManager.GetInstance(ctx, request.Id)
	if err != nil {
		switch {
		case errors.Is(err, instances.ErrNotFound):
			return oapi.StopInstance404JSONResponse{
				Code:    "not_found",
				Message: "instance not found",
			}, nil
		case errors.Is(err, instances.ErrAmbiguousName):
			return oapi.StopInstance404JSONResponse{
				Code:    "ambiguous",
				Message: "multiple instances match, use full instance ID",
			}, nil
		default:
			log.ErrorContext(ctx, "failed to get instance", "error", err, "id", request.Id)
			return oapi.StopInstance500JSONResponse{
				Code:    "internal_error",
				Message: "failed to get instance",
			}, nil
		}
	}

	inst, err := s.InstanceManager.StopInstance(ctx, resolved.Id)
	if err != nil {
		switch {
		case errors.Is(err, instances.ErrInvalidState):
			return oapi.StopInstance409JSONResponse{
				Code:    "invalid_state",
				Message: err.Error(),
			}, nil
		default:
			log.ErrorContext(ctx, "failed to stop instance", "error", err, "id", resolved.Id)
			return oapi.StopInstance500JSONResponse{
				Code:    "internal_error",
				Message: "failed to stop instance",
			}, nil
		}
	}
	return oapi.StopInstance200JSONResponse(instanceToOAPI(*inst)), nil
}

// StartInstance starts a stopped instance
// The id parameter can be an instance ID, name, or ID prefix
func (s *ApiService) StartInstance(ctx context.Context, request oapi.StartInstanceRequestObject) (oapi.StartInstanceResponseObject, error) {
	log := logger.FromContext(ctx)

	// Resolve to get the actual instance ID
	resolved, err := s.InstanceManager.GetInstance(ctx, request.Id)
	if err != nil {
		switch {
		case errors.Is(err, instances.ErrNotFound):
			return oapi.StartInstance404JSONResponse{
				Code:    "not_found",
				Message: "instance not found",
			}, nil
		case errors.Is(err, instances.ErrAmbiguousName):
			return oapi.StartInstance404JSONResponse{
				Code:    "ambiguous",
				Message: "multiple instances match, use full instance ID",
			}, nil
		default:
			log.ErrorContext(ctx, "failed to get instance", "error", err, "id", request.Id)
			return oapi.StartInstance500JSONResponse{
				Code:    "internal_error",
				Message: "failed to get instance",
			}, nil
		}
	}

	inst, err := s.InstanceManager.StartInstance(ctx, resolved.Id)
	if err != nil {
		switch {
		case errors.Is(err, instances.ErrInvalidState):
			return oapi.StartInstance409JSONResponse{
				Code:    "invalid_state",
				Message: err.Error(),
			}, nil
		default:
			log.ErrorContext(ctx, "failed to start instance", "error", err, "id", resolved.Id)
			return oapi.StartInstance500JSONResponse{
				Code:    "internal_error",
				Message: "failed to start instance",
			}, nil
		}
	}
	return oapi.StartInstance200JSONResponse(instanceToOAPI(*inst)), nil
}

// RebootInstance reboots a running instance
// The id parameter can be an instance ID, name, or ID prefix
func (s *ApiService) RebootInstance(ctx context.Context, request oapi.RebootInstanceRequestObject) (oapi.RebootInstanceResponseObject, error) {
	log := logger.FromContext(ctx)

	// Resolve to get the actual instance ID
	resolved, err := s.InstanceManager.GetInstance(ctx, request.Id)
	if err != nil {
		switch {
		case errors.Is(err, instances.ErrNotFound):
			return oapi.RebootInstance404JSONResponse{
				Code:    "not_found",
				Message: "instance not found",
			}, nil
		case errors.Is(err, instances.ErrAmbiguousName):
			return oapi.RebootInstance404JSONResponse{
				Code:    "ambiguous",
				Message: "multiple instances match, use full instance ID",
			}, nil
		default:
			log.ErrorContext(ctx, "failed to get instance", "error", err, "id", request.Id)
			return oapi.RebootInstance500JSONResponse{
				Code:    "internal_error",
				Message: "failed to get instance",
			}, nil
		}
	}

	inst, err := s.InstanceManager.RebootInstance(ctx, resolved.Id)
	if err != nil {
		switch {
		case errors.Is(err, instances.ErrInvalidState):
			return oapi.RebootInstance409JSONResponse{
				Code:    "invalid_state",
				Message: err.Error(),
			}, nil
		default:
			log.ErrorContext(ctx, "failed to reboot instance", "error", err, "id", resolved.Id)
			return oapi.RebootInstance500JSONResponse{
				Code:    "internal_error",
				Message: "failed to reboot instance",
			}, nil
		}
	}
	return oapi.RebootInstance200JSONResponse(instanceToOAPI(*inst)), nil
}

// logsStreamResponse implements oapi.GetInstanceLogsResponseObject with proper SSE flushing
type logsStreamResponse struct {
	logChan <-chan string
}

func (r logsStreamResponse) VisitGetInstanceLogsResponse(w http.ResponseWriter) error {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no") // Disable nginx buffering
	w.WriteHeader(200)

	flusher, ok := w.(http.Flusher)
	if !ok {
		return fmt.Errorf("streaming not supported")
	}

	for line := range r.logChan {
		jsonLine, _ := json.Marshal(line)
		fmt.Fprintf(w, "data: %s\n\n", jsonLine)
		flusher.Flush()
	}
	return nil
}

// GetInstanceLogs streams instance logs via SSE
// With follow=false (default), streams last N lines then closes
// With follow=true, streams last N lines then continues following new output
// The id parameter can be an instance ID, name, or ID prefix
func (s *ApiService) GetInstanceLogs(ctx context.Context, request oapi.GetInstanceLogsRequestObject) (oapi.GetInstanceLogsResponseObject, error) {
	tail := 100
	if request.Params.Tail != nil {
		tail = *request.Params.Tail
	}

	follow := false
	if request.Params.Follow != nil {
		follow = *request.Params.Follow
	}

	// Resolve to get the actual instance ID
	resolved, err := s.InstanceManager.GetInstance(ctx, request.Id)
	if err != nil {
		switch {
		case errors.Is(err, instances.ErrNotFound):
			return oapi.GetInstanceLogs404JSONResponse{
				Code:    "not_found",
				Message: "instance not found",
			}, nil
		case errors.Is(err, instances.ErrAmbiguousName):
			return oapi.GetInstanceLogs404JSONResponse{
				Code:    "ambiguous",
				Message: "multiple instances match, use full instance ID",
			}, nil
		default:
			return oapi.GetInstanceLogs500JSONResponse{
				Code:    "internal_error",
				Message: "failed to get instance",
			}, nil
		}
	}

	logChan, err := s.InstanceManager.StreamInstanceLogs(ctx, resolved.Id, tail, follow)
	if err != nil {
		switch {
		case errors.Is(err, instances.ErrTailNotFound):
			return oapi.GetInstanceLogs500JSONResponse{
				Code:    "dependency_missing",
				Message: "tail command not found on server - required for log streaming",
			}, nil
		default:
			return oapi.GetInstanceLogs500JSONResponse{
				Code:    "internal_error",
				Message: "failed to stream logs",
			}, nil
		}
	}

	return logsStreamResponse{logChan: logChan}, nil
}

// AttachVolume attaches a volume to an instance (not yet implemented)
func (s *ApiService) AttachVolume(ctx context.Context, request oapi.AttachVolumeRequestObject) (oapi.AttachVolumeResponseObject, error) {
	return oapi.AttachVolume500JSONResponse{
		Code:    "not_implemented",
		Message: "volume attachment not yet implemented",
	}, nil
}

// DetachVolume detaches a volume from an instance (not yet implemented)
func (s *ApiService) DetachVolume(ctx context.Context, request oapi.DetachVolumeRequestObject) (oapi.DetachVolumeResponseObject, error) {
	return oapi.DetachVolume500JSONResponse{
		Code:    "not_implemented",
		Message: "volume detachment not yet implemented",
	}, nil
}

// instanceToOAPI converts domain Instance to OAPI Instance
func instanceToOAPI(inst instances.Instance) oapi.Instance {
	// Format sizes as human-readable strings with best precision
	// HR() returns format like "1.5 GB" with 1 decimal place
	sizeStr := datasize.ByteSize(inst.Size).HR()
	hotplugSizeStr := datasize.ByteSize(inst.HotplugSize).HR()
	overlaySizeStr := datasize.ByteSize(inst.OverlaySize).HR()

	// Build network object with ip/mac nested inside
	netObj := &struct {
		Enabled *bool   `json:"enabled,omitempty"`
		Ip      *string `json:"ip"`
		Mac     *string `json:"mac"`
		Name    *string `json:"name,omitempty"`
	}{
		Enabled: lo.ToPtr(inst.NetworkEnabled),
	}
	if inst.NetworkEnabled {
		netObj.Name = lo.ToPtr("default")
		netObj.Ip = lo.ToPtr(inst.IP)
		netObj.Mac = lo.ToPtr(inst.MAC)
	}

	oapiInst := oapi.Instance{
		Id:          inst.Id,
		Name:        inst.Name,
		Image:       inst.Image,
		State:       oapi.InstanceState(inst.State),
		Size:        lo.ToPtr(sizeStr),
		HotplugSize: lo.ToPtr(hotplugSizeStr),
		OverlaySize: lo.ToPtr(overlaySizeStr),
		Vcpus:       lo.ToPtr(inst.Vcpus),
		Network:     netObj,
		CreatedAt:   inst.CreatedAt,
		StartedAt:   inst.StartedAt,
		StoppedAt:   inst.StoppedAt,
		HasSnapshot: lo.ToPtr(inst.HasSnapshot),
	}

	if len(inst.Env) > 0 {
		oapiInst.Env = &inst.Env
	}

	// Convert volume attachments
	if len(inst.Volumes) > 0 {
		oapiVolumes := make([]oapi.VolumeAttachment, len(inst.Volumes))
		for i, vol := range inst.Volumes {
			oapiVol := oapi.VolumeAttachment{
				VolumeId:  vol.VolumeID,
				MountPath: vol.MountPath,
				Readonly:  lo.ToPtr(vol.Readonly),
			}
			if vol.Overlay {
				oapiVol.Overlay = lo.ToPtr(true)
				overlaySizeStr := datasize.ByteSize(vol.OverlaySize).HR()
				oapiVol.OverlaySize = lo.ToPtr(overlaySizeStr)
			}
			oapiVolumes[i] = oapiVol
		}
		oapiInst.Volumes = &oapiVolumes
	}

	return oapiInst
}
