package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/c2h5oh/datasize"
	"github.com/kernel/hypeman/lib/guest"
	"github.com/kernel/hypeman/lib/hypervisor"
	"github.com/kernel/hypeman/lib/instances"
	"github.com/kernel/hypeman/lib/logger"
	mw "github.com/kernel/hypeman/lib/middleware"
	"github.com/kernel/hypeman/lib/network"
	"github.com/kernel/hypeman/lib/oapi"
	"github.com/kernel/hypeman/lib/resources"
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

	// Parse disk_io_bps (0 = auto/unlimited)
	diskIOBps := int64(0)
	if request.Body.DiskIoBps != nil && *request.Body.DiskIoBps != "" {
		var ioBpsBytes datasize.ByteSize
		// Remove "/s" suffix if present
		ioStr := *request.Body.DiskIoBps
		ioStr = strings.TrimSuffix(ioStr, "/s")
		ioStr = strings.TrimSuffix(ioStr, "ps")
		if err := ioBpsBytes.UnmarshalText([]byte(ioStr)); err != nil {
			return oapi.CreateInstance400JSONResponse{
				Code:    "invalid_disk_io_bps",
				Message: fmt.Sprintf("invalid disk_io_bps format: %v", err),
			}, nil
		}
		diskIOBps = int64(ioBpsBytes)
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

	// Parse network bandwidth limits (0 = auto)
	// Supports both bit-based (e.g., "1Gbps") and byte-based (e.g., "125MB/s") formats
	var networkBandwidthDownload int64
	var networkBandwidthUpload int64
	if request.Body.Network != nil {
		if request.Body.Network.BandwidthDownload != nil && *request.Body.Network.BandwidthDownload != "" {
			bw, err := resources.ParseBandwidth(*request.Body.Network.BandwidthDownload)
			if err != nil {
				return oapi.CreateInstance400JSONResponse{
					Code:    "invalid_bandwidth_download",
					Message: fmt.Sprintf("invalid bandwidth_download format: %v", err),
				}, nil
			}
			networkBandwidthDownload = bw
		}
		if request.Body.Network.BandwidthUpload != nil && *request.Body.Network.BandwidthUpload != "" {
			bw, err := resources.ParseBandwidth(*request.Body.Network.BandwidthUpload)
			if err != nil {
				return oapi.CreateInstance400JSONResponse{
					Code:    "invalid_bandwidth_upload",
					Message: fmt.Sprintf("invalid bandwidth_upload format: %v", err),
				}, nil
			}
			networkBandwidthUpload = bw
		}
	}

	// Parse devices (GPU passthrough)
	var deviceRefs []string
	if request.Body.Devices != nil {
		deviceRefs = *request.Body.Devices
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

	// Convert hypervisor type from API enum to domain type
	var hvType hypervisor.Type
	if request.Body.Hypervisor != nil {
		hvType = hypervisor.Type(*request.Body.Hypervisor)
	}

	// Calculate default resource limits when not specified (0 = auto)
	// Uses proportional allocation based on CPU: (vcpus / cpuCapacity) * resourceCapacity
	if diskIOBps == 0 {
		diskIOBps, _ = s.ResourceManager.DefaultDiskIOBandwidth(vcpus)
	}
	if networkBandwidthDownload == 0 || networkBandwidthUpload == 0 {
		defaultDown, defaultUp := s.ResourceManager.DefaultNetworkBandwidth(vcpus)
		if networkBandwidthDownload == 0 {
			networkBandwidthDownload = defaultDown
		}
		if networkBandwidthUpload == 0 {
			networkBandwidthUpload = defaultUp
		}
	}

	domainReq := instances.CreateInstanceRequest{
		Name:                     request.Body.Name,
		Image:                    request.Body.Image,
		Size:                     size,
		HotplugSize:              hotplugSize,
		OverlaySize:              overlaySize,
		Vcpus:                    vcpus,
		DiskIOBps:                diskIOBps,
		NetworkBandwidthDownload: networkBandwidthDownload,
		NetworkBandwidthUpload:   networkBandwidthUpload,
		Env:                      env,
		NetworkEnabled:           networkEnabled,
		Devices:                  deviceRefs,
		Volumes:                  volumes,
		Hypervisor:               hvType,
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
// Note: Resolution is handled by ResolveResource middleware
func (s *ApiService) GetInstance(ctx context.Context, request oapi.GetInstanceRequestObject) (oapi.GetInstanceResponseObject, error) {
	inst := mw.GetResolvedInstance[instances.Instance](ctx)
	if inst == nil {
		return oapi.GetInstance500JSONResponse{
			Code:    "internal_error",
			Message: "resource not resolved",
		}, nil
	}
	return oapi.GetInstance200JSONResponse(instanceToOAPI(*inst)), nil
}

// DeleteInstance stops and deletes an instance
// The id parameter can be an instance ID, name, or ID prefix
// Note: Resolution is handled by ResolveResource middleware
func (s *ApiService) DeleteInstance(ctx context.Context, request oapi.DeleteInstanceRequestObject) (oapi.DeleteInstanceResponseObject, error) {
	inst := mw.GetResolvedInstance[instances.Instance](ctx)
	if inst == nil {
		return oapi.DeleteInstance500JSONResponse{
			Code:    "internal_error",
			Message: "resource not resolved",
		}, nil
	}
	log := logger.FromContext(ctx)

	err := s.InstanceManager.DeleteInstance(ctx, inst.Id)
	if err != nil {
		log.ErrorContext(ctx, "failed to delete instance", "error", err)
		return oapi.DeleteInstance500JSONResponse{
			Code:    "internal_error",
			Message: "failed to delete instance",
		}, nil
	}
	return oapi.DeleteInstance204Response{}, nil
}

// StandbyInstance puts an instance in standby (pause, snapshot, delete VMM)
// The id parameter can be an instance ID, name, or ID prefix
// Note: Resolution is handled by ResolveResource middleware
func (s *ApiService) StandbyInstance(ctx context.Context, request oapi.StandbyInstanceRequestObject) (oapi.StandbyInstanceResponseObject, error) {
	inst := mw.GetResolvedInstance[instances.Instance](ctx)
	if inst == nil {
		return oapi.StandbyInstance500JSONResponse{
			Code:    "internal_error",
			Message: "resource not resolved",
		}, nil
	}
	log := logger.FromContext(ctx)

	result, err := s.InstanceManager.StandbyInstance(ctx, inst.Id)
	if err != nil {
		switch {
		case errors.Is(err, instances.ErrInvalidState):
			return oapi.StandbyInstance409JSONResponse{
				Code:    "invalid_state",
				Message: err.Error(),
			}, nil
		default:
			log.ErrorContext(ctx, "failed to standby instance", "error", err)
			return oapi.StandbyInstance500JSONResponse{
				Code:    "internal_error",
				Message: "failed to standby instance",
			}, nil
		}
	}
	return oapi.StandbyInstance200JSONResponse(instanceToOAPI(*result)), nil
}

// RestoreInstance restores an instance from standby
// The id parameter can be an instance ID, name, or ID prefix
// Note: Resolution is handled by ResolveResource middleware
func (s *ApiService) RestoreInstance(ctx context.Context, request oapi.RestoreInstanceRequestObject) (oapi.RestoreInstanceResponseObject, error) {
	inst := mw.GetResolvedInstance[instances.Instance](ctx)
	if inst == nil {
		return oapi.RestoreInstance500JSONResponse{
			Code:    "internal_error",
			Message: "resource not resolved",
		}, nil
	}
	log := logger.FromContext(ctx)

	result, err := s.InstanceManager.RestoreInstance(ctx, inst.Id)
	if err != nil {
		switch {
		case errors.Is(err, instances.ErrInvalidState):
			return oapi.RestoreInstance409JSONResponse{
				Code:    "invalid_state",
				Message: err.Error(),
			}, nil
		default:
			log.ErrorContext(ctx, "failed to restore instance", "error", err)
			return oapi.RestoreInstance500JSONResponse{
				Code:    "internal_error",
				Message: "failed to restore instance",
			}, nil
		}
	}
	return oapi.RestoreInstance200JSONResponse(instanceToOAPI(*result)), nil
}

// StopInstance gracefully stops a running instance
// The id parameter can be an instance ID, name, or ID prefix
// Note: Resolution is handled by ResolveResource middleware
func (s *ApiService) StopInstance(ctx context.Context, request oapi.StopInstanceRequestObject) (oapi.StopInstanceResponseObject, error) {
	inst := mw.GetResolvedInstance[instances.Instance](ctx)
	if inst == nil {
		return oapi.StopInstance500JSONResponse{
			Code:    "internal_error",
			Message: "resource not resolved",
		}, nil
	}
	log := logger.FromContext(ctx)

	result, err := s.InstanceManager.StopInstance(ctx, inst.Id)
	if err != nil {
		switch {
		case errors.Is(err, instances.ErrInvalidState):
			return oapi.StopInstance409JSONResponse{
				Code:    "invalid_state",
				Message: err.Error(),
			}, nil
		default:
			log.ErrorContext(ctx, "failed to stop instance", "error", err)
			return oapi.StopInstance500JSONResponse{
				Code:    "internal_error",
				Message: "failed to stop instance",
			}, nil
		}
	}
	return oapi.StopInstance200JSONResponse(instanceToOAPI(*result)), nil
}

// StartInstance starts a stopped instance
// The id parameter can be an instance ID, name, or ID prefix
// Note: Resolution is handled by ResolveResource middleware
func (s *ApiService) StartInstance(ctx context.Context, request oapi.StartInstanceRequestObject) (oapi.StartInstanceResponseObject, error) {
	inst := mw.GetResolvedInstance[instances.Instance](ctx)
	if inst == nil {
		return oapi.StartInstance500JSONResponse{
			Code:    "internal_error",
			Message: "resource not resolved",
		}, nil
	}
	log := logger.FromContext(ctx)

	result, err := s.InstanceManager.StartInstance(ctx, inst.Id)
	if err != nil {
		switch {
		case errors.Is(err, instances.ErrInvalidState):
			return oapi.StartInstance409JSONResponse{
				Code:    "invalid_state",
				Message: err.Error(),
			}, nil
		default:
			log.ErrorContext(ctx, "failed to start instance", "error", err)
			return oapi.StartInstance500JSONResponse{
				Code:    "internal_error",
				Message: "failed to start instance",
			}, nil
		}
	}
	return oapi.StartInstance200JSONResponse(instanceToOAPI(*result)), nil
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
// Note: Resolution is handled by ResolveResource middleware
func (s *ApiService) GetInstanceLogs(ctx context.Context, request oapi.GetInstanceLogsRequestObject) (oapi.GetInstanceLogsResponseObject, error) {
	inst := mw.GetResolvedInstance[instances.Instance](ctx)
	if inst == nil {
		return oapi.GetInstanceLogs500JSONResponse{
			Code:    "internal_error",
			Message: "resource not resolved",
		}, nil
	}

	tail := 100
	if request.Params.Tail != nil {
		tail = *request.Params.Tail
	}

	follow := false
	if request.Params.Follow != nil {
		follow = *request.Params.Follow
	}

	// Map source parameter to LogSource type (default to app)
	source := instances.LogSourceApp
	if request.Params.Source != nil {
		switch *request.Params.Source {
		case oapi.App:
			source = instances.LogSourceApp
		case oapi.Vmm:
			source = instances.LogSourceVMM
		case oapi.Hypeman:
			source = instances.LogSourceHypeman
		}
	}

	logChan, err := s.InstanceManager.StreamInstanceLogs(ctx, inst.Id, tail, follow, source)
	if err != nil {
		switch {
		case errors.Is(err, instances.ErrTailNotFound):
			return oapi.GetInstanceLogs500JSONResponse{
				Code:    "dependency_missing",
				Message: "tail command not found on server - required for log streaming",
			}, nil
		case errors.Is(err, instances.ErrLogNotFound):
			return oapi.GetInstanceLogs404JSONResponse{
				Code:    "log_not_found",
				Message: "requested log file does not exist yet",
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

// StatInstancePath returns information about a path in the guest filesystem
// The id parameter can be an instance ID, name, or ID prefix
// Note: Resolution is handled by ResolveResource middleware
func (s *ApiService) StatInstancePath(ctx context.Context, request oapi.StatInstancePathRequestObject) (oapi.StatInstancePathResponseObject, error) {
	log := logger.FromContext(ctx)

	inst := mw.GetResolvedInstance[instances.Instance](ctx)
	if inst == nil {
		return oapi.StatInstancePath500JSONResponse{
			Code:    "internal_error",
			Message: "resource not resolved",
		}, nil
	}

	if inst.State != instances.StateRunning {
		return oapi.StatInstancePath409JSONResponse{
			Code:    "invalid_state",
			Message: fmt.Sprintf("instance must be running (current state: %s)", inst.State),
		}, nil
	}

	// Create vsock dialer for this hypervisor type
	dialer, err := hypervisor.NewVsockDialer(inst.HypervisorType, inst.VsockSocket, inst.VsockCID)
	if err != nil {
		log.ErrorContext(ctx, "failed to create vsock dialer", "error", err)
		return oapi.StatInstancePath500JSONResponse{
			Code:    "internal_error",
			Message: "failed to create vsock dialer",
		}, nil
	}

	grpcConn, err := guest.GetOrCreateConn(ctx, dialer)
	if err != nil {
		log.ErrorContext(ctx, "failed to get grpc connection", "error", err)
		return oapi.StatInstancePath500JSONResponse{
			Code:    "internal_error",
			Message: "failed to connect to guest agent",
		}, nil
	}

	client := guest.NewGuestServiceClient(grpcConn)
	followLinks := false
	if request.Params.FollowLinks != nil {
		followLinks = *request.Params.FollowLinks
	}

	resp, err := client.StatPath(ctx, &guest.StatPathRequest{
		Path:        request.Params.Path,
		FollowLinks: followLinks,
	})
	if err != nil {
		log.ErrorContext(ctx, "stat path failed", "error", err, "path", request.Params.Path)
		return oapi.StatInstancePath500JSONResponse{
			Code:    "internal_error",
			Message: "failed to stat path in guest",
		}, nil
	}

	// Convert types from protobuf to OAPI
	mode := int(resp.Mode)
	response := oapi.StatInstancePath200JSONResponse{
		Exists:     resp.Exists,
		IsDir:      &resp.IsDir,
		IsFile:     &resp.IsFile,
		IsSymlink:  &resp.IsSymlink,
		LinkTarget: &resp.LinkTarget,
		Mode:       &mode,
		Size:       &resp.Size,
	}
	// Include error message if stat failed (e.g., permission denied)
	if resp.Error != "" {
		response.Error = &resp.Error
	}
	return response, nil
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

	// Format bandwidth as human-readable (bytes/s to rate string)
	var downloadBwStr, uploadBwStr *string
	if inst.NetworkBandwidthDownload > 0 {
		s := datasize.ByteSize(inst.NetworkBandwidthDownload).HR() + "/s"
		downloadBwStr = &s
	}
	if inst.NetworkBandwidthUpload > 0 {
		s := datasize.ByteSize(inst.NetworkBandwidthUpload).HR() + "/s"
		uploadBwStr = &s
	}

	// Build network object with ip/mac and bandwidth nested inside
	netObj := &struct {
		BandwidthDownload *string `json:"bandwidth_download,omitempty"`
		BandwidthUpload   *string `json:"bandwidth_upload,omitempty"`
		Enabled           *bool   `json:"enabled,omitempty"`
		Ip                *string `json:"ip"`
		Mac               *string `json:"mac"`
		Name              *string `json:"name,omitempty"`
	}{
		Enabled:           lo.ToPtr(inst.NetworkEnabled),
		BandwidthDownload: downloadBwStr,
		BandwidthUpload:   uploadBwStr,
	}
	if inst.NetworkEnabled {
		netObj.Name = lo.ToPtr("default")
		netObj.Ip = lo.ToPtr(inst.IP)
		netObj.Mac = lo.ToPtr(inst.MAC)
	}

	// Convert hypervisor type
	hvType := oapi.InstanceHypervisor(inst.HypervisorType)

	// Format disk I/O as human-readable
	var diskIoBpsStr *string
	if inst.DiskIOBps > 0 {
		s := datasize.ByteSize(inst.DiskIOBps).HR() + "/s"
		diskIoBpsStr = &s
	}

	oapiInst := oapi.Instance{
		Id:          inst.Id,
		Name:        inst.Name,
		Image:       inst.Image,
		State:       oapi.InstanceState(inst.State),
		StateError:  inst.StateError,
		Size:        lo.ToPtr(sizeStr),
		HotplugSize: lo.ToPtr(hotplugSizeStr),
		OverlaySize: lo.ToPtr(overlaySizeStr),
		Vcpus:       lo.ToPtr(inst.Vcpus),
		DiskIoBps:   diskIoBpsStr,
		Network:     netObj,
		CreatedAt:   inst.CreatedAt,
		StartedAt:   inst.StartedAt,
		StoppedAt:   inst.StoppedAt,
		HasSnapshot: lo.ToPtr(inst.HasSnapshot),
		Hypervisor:  &hvType,
	}

	if len(inst.Env) > 0 {
		oapiInst.Env = &inst.Env
	}

	// Convert volume attachments
	if len(inst.Volumes) > 0 {
		oapiVolumes := make([]oapi.VolumeMount, len(inst.Volumes))
		for i, vol := range inst.Volumes {
			oapiVol := oapi.VolumeMount{
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
