package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"

	"github.com/c2h5oh/datasize"
	"github.com/go-chi/chi/v5"
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
		log.Error("failed to list instances", "error", err)
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

	domainReq := instances.CreateInstanceRequest{
		Name:           request.Body.Name,
		Image:          request.Body.Image,
		Size:           size,
		HotplugSize:    hotplugSize,
		OverlaySize:    overlaySize,
		Vcpus:          vcpus,
		Env:            env,
		NetworkEnabled: networkEnabled,
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
			log.Error("failed to create instance", "error", err, "image", request.Body.Image)
			return oapi.CreateInstance500JSONResponse{
				Code:    "internal_error",
				Message: "failed to create instance",
			}, nil
		}
	}
	return oapi.CreateInstance201JSONResponse(instanceToOAPI(*inst)), nil
}

// GetInstance gets instance details
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
		default:
			log.Error("failed to get instance", "error", err, "id", request.Id)
			return oapi.GetInstance500JSONResponse{
				Code:    "internal_error",
				Message: "failed to get instance",
			}, nil
		}
	}
	return oapi.GetInstance200JSONResponse(instanceToOAPI(*inst)), nil
}

// DeleteInstance stops and deletes an instance
func (s *ApiService) DeleteInstance(ctx context.Context, request oapi.DeleteInstanceRequestObject) (oapi.DeleteInstanceResponseObject, error) {
	log := logger.FromContext(ctx)

	err := s.InstanceManager.DeleteInstance(ctx, request.Id)
	if err != nil {
		switch {
		case errors.Is(err, instances.ErrNotFound):
			return oapi.DeleteInstance404JSONResponse{
				Code:    "not_found",
				Message: "instance not found",
			}, nil
		default:
			log.Error("failed to delete instance", "error", err, "id", request.Id)
			return oapi.DeleteInstance500JSONResponse{
				Code:    "internal_error",
				Message: "failed to delete instance",
			}, nil
		}
	}
	return oapi.DeleteInstance204Response{}, nil
}

// StandbyInstance puts an instance in standby (pause, snapshot, delete VMM)
func (s *ApiService) StandbyInstance(ctx context.Context, request oapi.StandbyInstanceRequestObject) (oapi.StandbyInstanceResponseObject, error) {
	log := logger.FromContext(ctx)

	inst, err := s.InstanceManager.StandbyInstance(ctx, request.Id)
	if err != nil {
		switch {
		case errors.Is(err, instances.ErrNotFound):
			return oapi.StandbyInstance404JSONResponse{
				Code:    "not_found",
				Message: "instance not found",
			}, nil
		case errors.Is(err, instances.ErrInvalidState):
			return oapi.StandbyInstance409JSONResponse{
				Code:    "invalid_state",
				Message: err.Error(),
			}, nil
		default:
			log.Error("failed to standby instance", "error", err, "id", request.Id)
			return oapi.StandbyInstance500JSONResponse{
				Code:    "internal_error",
				Message: "failed to standby instance",
			}, nil
		}
	}
	return oapi.StandbyInstance200JSONResponse(instanceToOAPI(*inst)), nil
}

// RestoreInstance restores an instance from standby
func (s *ApiService) RestoreInstance(ctx context.Context, request oapi.RestoreInstanceRequestObject) (oapi.RestoreInstanceResponseObject, error) {
	log := logger.FromContext(ctx)

	inst, err := s.InstanceManager.RestoreInstance(ctx, request.Id)
	if err != nil {
		switch {
		case errors.Is(err, instances.ErrNotFound):
			return oapi.RestoreInstance404JSONResponse{
				Code:    "not_found",
				Message: "instance not found",
			}, nil
		case errors.Is(err, instances.ErrInvalidState):
			return oapi.RestoreInstance409JSONResponse{
				Code:    "invalid_state",
				Message: err.Error(),
			}, nil
		default:
			log.Error("failed to restore instance", "error", err, "id", request.Id)
			return oapi.RestoreInstance500JSONResponse{
				Code:    "internal_error",
				Message: "failed to restore instance",
			}, nil
		}
	}
	return oapi.RestoreInstance200JSONResponse(instanceToOAPI(*inst)), nil
}

// LogsHandler streams instance logs via SSE (Server-Sent Events)
// This handler is registered outside the timeout middleware for long-running connections
// With follow=false (default), streams last N lines then closes
// With follow=true, streams last N lines then continues following new output
func (s *ApiService) LogsHandler(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	log := logger.FromContext(ctx)

	instanceID := chi.URLParam(r, "id")

	// Parse tail parameter
	tail := 100
	if tailStr := r.URL.Query().Get("tail"); tailStr != "" {
		if parsed, err := strconv.Atoi(tailStr); err == nil && parsed > 0 {
			tail = parsed
		}
	}

	// Parse follow parameter
	follow := r.URL.Query().Get("follow") == "true"

	// Start streaming logs
	logChan, err := s.InstanceManager.StreamInstanceLogs(ctx, instanceID, tail, follow)
	if err != nil {
		if errors.Is(err, instances.ErrNotFound) {
			http.Error(w, `{"code":"not_found","message":"instance not found"}`, http.StatusNotFound)
			return
		}
		log.ErrorContext(ctx, "failed to start log stream", "error", err, "id", instanceID)
		http.Error(w, `{"code":"internal_error","message":"failed to stream logs"}`, http.StatusInternalServerError)
		return
	}

	// Set SSE headers
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no") // Disable nginx buffering

	// Get flusher for streaming
	flusher, ok := w.(http.Flusher)
	if !ok {
		log.ErrorContext(ctx, "streaming not supported")
		http.Error(w, `{"code":"internal_error","message":"streaming not supported"}`, http.StatusInternalServerError)
		return
	}

	// Stream logs as SSE events
	for {
		select {
		case <-ctx.Done():
			log.DebugContext(ctx, "client disconnected", "id", instanceID)
			return
		case line, ok := <-logChan:
			if !ok {
				// Channel closed, stream ended (tail finished without follow)
				log.DebugContext(ctx, "log stream ended", "id", instanceID)
				return
			}
			// Write SSE formatted event
			fmt.Fprintf(w, "data: %s\n\n", line)
			flusher.Flush()
		}
	}
}

// GetInstanceLogs implements the strict server interface method
// This is a stub - the actual handler is LogsHandler which is registered
// outside the timeout middleware
func (s *ApiService) GetInstanceLogs(ctx context.Context, request oapi.GetInstanceLogsRequestObject) (oapi.GetInstanceLogsResponseObject, error) {
	// This should never be called as LogsHandler takes precedence in routing
	return oapi.GetInstanceLogs500JSONResponse{
		Code:    "internal_error",
		Message: "logs endpoint handled by LogsHandler",
	}, nil
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

	return oapiInst
}
