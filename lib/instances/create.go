package instances

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"time"

	"github.com/kernel/hypeman/lib/devices"
	"github.com/kernel/hypeman/lib/egressproxy"
	"github.com/kernel/hypeman/lib/guestmemory"
	"github.com/kernel/hypeman/lib/hypervisor"
	"github.com/kernel/hypeman/lib/images"
	"github.com/kernel/hypeman/lib/instances/phasetracking"
	"github.com/kernel/hypeman/lib/logger"
	"github.com/kernel/hypeman/lib/network"
	"github.com/kernel/hypeman/lib/system"
	"github.com/kernel/hypeman/lib/tags"
	"github.com/kernel/hypeman/lib/volumes"
	"github.com/nrednav/cuid2"
	"go.opentelemetry.io/otel/attribute"
	"gvisor.dev/gvisor/pkg/cleanup"
)

const (
	// MaxVolumesPerInstance is the maximum number of volumes that can be attached
	// to a single instance. This limit exists because volume devices are named
	// /dev/vdd, /dev/vde, ... /dev/vdz (letters d-z = 23 devices).
	// Devices a-c are reserved for rootfs, overlay, and config disk.
	MaxVolumesPerInstance = 23
)

// systemDirectories are paths that cannot be used as volume mount points
var systemDirectories = []string{
	"/",
	"/bin",
	"/boot",
	"/dev",
	"/etc",
	"/lib",
	"/lib64",
	"/proc",
	"/root",
	"/run",
	"/sbin",
	"/sys",
	"/tmp",
	"/usr",
	"/var",
}

func wrapCreateVGPUErr(profile string, err error) error {
	if errors.Is(err, devices.ErrVGPUNotSupportedOnMacOS) {
		return fmt.Errorf("%w: %w", ErrInvalidRequest, err)
	}
	return fmt.Errorf("create vGPU mdev for profile %s: %w", profile, err)
}

// generateVsockCID converts first 8 chars of instance ID to a unique CID
// CIDs 0-2 are reserved (hypervisor, loopback, host)
// Returns value in range 3 to 4294967295
func generateVsockCID(instanceID string) int64 {
	idPrefix := instanceID
	if len(idPrefix) > 8 {
		idPrefix = idPrefix[:8]
	}

	var sum int64
	for _, c := range idPrefix {
		sum = sum*37 + int64(c)
	}

	return (sum % 4294967292) + 3
}

// createInstance creates and starts a new instance
// Multi-hop orchestration: Stopped → Created → Running
func (m *manager) createInstance(
	ctx context.Context,
	req CreateInstanceRequest,
) (_ *Instance, retErr error) {
	start := time.Now()
	log := logger.FromContext(ctx)
	log.InfoContext(ctx, "creating instance", "name", req.Name, "image", req.Image, "vcpus", req.Vcpus)

	ctx, span := m.startLifecycleSpan(ctx, "instances.create",
		attribute.String("operation", "create"),
	)
	defer func() { finishInstancesSpan(span, retErr) }()

	// 1. Validate request
	if err := validateCreateRequest(&req); err != nil {
		log.ErrorContext(ctx, "invalid create request", "error", err)
		return nil, err
	}
	if err := validateExpiresAt(req.ExpiresAt, m.nowUTC()); err != nil {
		return nil, err
	}
	if req.GPU != nil && req.GPU.Profile != "" && !devices.Capabilities().SupportsVGPU {
		return nil, fmt.Errorf("%w: %w", ErrInvalidRequest, devices.ErrVGPUNotSupportedOnMacOS)
	}
	hvType := req.Hypervisor
	if hvType == "" {
		hvType = m.defaultHypervisor
	}
	starter, starterErr := m.getVMStarter(hvType)
	if starterErr == nil {
		if err := m.validateCreateVMConfig(starter, req, hvType); err != nil {
			return nil, err
		}
	}

	// 2. Validate image exists and is ready; auto-pull if not found
	log.DebugContext(ctx, "validating image", "image", req.Image)
	imageCtx, imageSpanEnd := m.startLifecycleStep(ctx, "resolve_image",
		attribute.String("operation", "resolve_image"),
	)
	imageInfo, err := resolveImageForCreate(imageCtx, m.imageManager, req.Image, req.Platform, log)
	if err != nil {
		imageSpanEnd(err)
		log.ErrorContext(ctx, "failed to resolve image", "image", req.Image, "platform", req.Platform, "error", err)
		return nil, err
	}

	imageSpanEnd(nil)

	if imageInfo.Status != images.StatusReady {
		log.ErrorContext(ctx, "image not ready", "image", req.Image, "status", imageInfo.Status)
		return nil, fmt.Errorf("%w: image status is %s", ErrImageNotReady, imageInfo.Status)
	}

	// A guest whose architecture differs from the host kernel can only boot via
	// emulation. On Apple silicon that is Rosetta, enabled automatically; on any
	// other host we reject the create up front rather than launching an
	// unbootable VM.
	enableRosetta, err := deriveEnableRosetta(images.ImageNeedsHostEmulation(imageInfo.Platform), hvType)
	if err != nil {
		log.ErrorContext(ctx, "image platform requires emulation", "image", req.Image, "image_platform", imageInfo.Platform, "host", hostOSArchString())
		return nil, err
	}
	m.recordImageUsage(ctx, imageInfo)

	// Preserve image-retention cleanup even if this manager cannot start the
	// requested backend. Backend-specific config errors were already rejected
	// before image resolution whenever a starter was available.
	if starterErr != nil {
		return nil, fmt.Errorf("get vm starter for %s: %w", hvType, starterErr)
	}

	defaultKernel := m.systemManager.GetDefaultKernelVersion()
	kernelVer, err := resolveCreateKernelVersion(imageInfo, defaultKernel)
	if err != nil {
		log.ErrorContext(ctx, "invalid image kernel label", "image", req.Image, "error", err)
		return nil, err
	}
	if kernelVer != defaultKernel {
		log.InfoContext(ctx, "using image-declared kernel version",
			"image", req.Image,
			"kernel", kernelVer,
			"label", system.ImageKernelVersionLabel)
	}
	// resolvedImageRef is the digest-pinned reference used for boot/start/restore
	// (stable across mutable tags). The caller-facing Image field keeps the
	// original reference the user supplied for display/addressing.
	resolvedImageRef, err := storedImageNameForCreate(req.Image, imageInfo)
	if err != nil {
		return nil, err
	}

	// 3. Generate instance ID (CUID2 for secure, collision-resistant IDs)
	id := cuid2.Generate()
	ctx = enrichInstancesTrace(ctx, attribute.String("instance_id", id))
	log.DebugContext(ctx, "generated instance ID", "instance_id", id)

	// Keep the reaper from deleting metadata while creation is still in flight.
	instanceLock, ownsInstanceLock := m.loadOrStoreInstanceLock(id)
	removeInstanceLockOnFailure := ownsInstanceLock
	instanceLock.Lock()
	defer func() {
		if retErr != nil && removeInstanceLockOnFailure {
			m.instanceLocks.CompareAndDelete(id, instanceLock)
		}
		instanceLock.Unlock()
	}()

	// 4. Generate vsock configuration
	vsockCID := generateVsockCID(id)
	vsockSocket := m.paths.InstanceSocket(id, hypervisor.VsockSocketNameForType(hvType))
	log.DebugContext(ctx, "generated vsock config", "instance_id", id, "cid", vsockCID)

	// 5. Check instance doesn't already exist
	if _, err := m.loadMetadata(id); err == nil {
		removeInstanceLockOnFailure = false
		return nil, ErrAlreadyExists
	}

	// 6. Apply defaults
	size := req.Size
	if size == 0 {
		size = 1 * 1024 * 1024 * 1024 // 1GB default
	}
	hotplugSize := req.HotplugSize
	overlaySize := req.OverlaySize
	if overlaySize == 0 {
		overlaySize = 10 * 1024 * 1024 * 1024 // 10GB default
	}
	// Validate overlay size against max
	if overlaySize > m.limits.MaxOverlaySize {
		return nil, fmt.Errorf("overlay size %d exceeds maximum allowed size %d", overlaySize, m.limits.MaxOverlaySize)
	}
	vcpus := req.Vcpus
	if vcpus == 0 {
		vcpus = 2
	}

	// Validate per-instance resource limits
	if m.limits.MaxVcpusPerInstance > 0 && vcpus > m.limits.MaxVcpusPerInstance {
		return nil, fmt.Errorf("vcpus %d exceeds maximum allowed %d per instance", vcpus, m.limits.MaxVcpusPerInstance)
	}
	totalMemory := size + hotplugSize
	if m.limits.MaxMemoryPerInstance > 0 && totalMemory > m.limits.MaxMemoryPerInstance {
		return nil, fmt.Errorf("total memory %d (size + hotplug_size) exceeds maximum allowed %d per instance", totalMemory, m.limits.MaxMemoryPerInstance)
	}

	diskBytes := requestedDiskReservationBytes(overlaySize, req.Volumes)
	reservedResources := false

	// Reserve aggregate resources for this create while it is in flight.
	if m.resourceValidator != nil {
		needsGPU := req.GPU != nil && req.GPU.Profile != ""
		if err := m.resourceValidator.ReserveAllocation(ctx, id, vcpus, totalMemory, req.NetworkBandwidthDownload, req.NetworkBandwidthUpload, req.DiskIOBps, diskBytes, needsGPU); err != nil {
			log.ErrorContext(ctx, "resource reservation failed", "error", err)
			return nil, fmt.Errorf("%w: %v", ErrInsufficientResources, err)
		}
		reservedResources = true
		defer func() {
			if reservedResources {
				m.resourceValidator.FinishAllocation(id)
			}
		}()
	}

	if req.Env == nil {
		req.Env = make(map[string]string)
	}
	if req.Tags == nil {
		req.Tags = make(map[string]string)
	}

	// 7. Determine network based on NetworkEnabled flag
	networkName := ""
	if req.NetworkEnabled {
		networkName = "default"
	}

	// Enrich logger and trace span with hypervisor type
	log = log.With("hypervisor", string(hvType))
	ctx = logger.AddToContext(ctx, log)
	ctx = enrichInstancesTrace(ctx, attribute.String("hypervisor", string(hvType)))

	hvVersion, err := m.resolveCreateHypervisorVersion(ctx, starter, hvType, req.HypervisorVersion)
	if err != nil {
		return nil, err
	}

	// 10. Validate, resolve, and auto-bind devices (GPU passthrough)
	// Track devices we've marked as attached for cleanup on error.
	// The cleanup closure captures this slice by reference, so it will see
	// whatever devices have been attached when cleanup runs.
	var attachedDeviceIDs []string
	var resolvedDeviceIDs []string
	var gpuDevice *devices.VGPUDevice
	var gpuProfile string
	var gpuFramework devices.VGPUFramework
	var gpuDevicePath string
	var gpuMdevUUID string

	// Setup cleanup stack early so device attachment errors trigger cleanup
	cu := cleanup.Make(func() {
		log.DebugContext(ctx, "cleaning up instance on error", "instance_id", id)
		m.deleteInstanceData(id)
	})
	defer cu.Clean()

	// Add device detachment cleanup - closure captures attachedDeviceIDs by reference
	if m.deviceManager != nil {
		cu.Add(func() {
			for _, deviceID := range attachedDeviceIDs {
				log.DebugContext(ctx, "detaching device on cleanup", "instance_id", id, "device", deviceID)
				m.deviceManager.MarkDetached(ctx, deviceID)
			}
		})
	}

	// Handle vGPU profile request
	if req.GPU != nil && req.GPU.Profile != "" {
		log.InfoContext(ctx, "creating vGPU", "instance_id", id, "profile", req.GPU.Profile)
		gpuDevice, err = devices.CreateVGPU(ctx, req.GPU.Profile, id)
		if err != nil {
			log.ErrorContext(ctx, "failed to create vGPU", "profile", req.GPU.Profile, "error", err)
			return nil, wrapCreateVGPUErr(req.GPU.Profile, err)
		}
		gpuProfile = gpuDevice.ProfileName
		gpuFramework = gpuDevice.Framework
		gpuDevicePath = gpuDevice.SysfsPath
		gpuMdevUUID = gpuDevice.MdevUUID
		log.InfoContext(ctx, "created vGPU", "instance_id", id, "profile", gpuProfile, "uuid", gpuMdevUUID)

		// Add vGPU cleanup to stack
		cu.Add(func() {
			log.DebugContext(ctx, "destroying vGPU on cleanup", "instance_id", id, "uuid", gpuDevice.MdevUUID)
			assignment := devices.VGPUAssignment{
				Framework:  gpuDevice.Framework,
				DevicePath: gpuDevice.SysfsPath,
				MdevUUID:   gpuDevice.MdevUUID,
			}
			if err := devices.DestroyVGPU(ctx, assignment); err != nil {
				log.WarnContext(ctx, "failed to destroy vGPU on cleanup", "instance_id", id, "uuid", gpuDevice.MdevUUID, "error", err)
			}
		})
	}

	if len(req.Devices) > 0 && m.deviceManager != nil {
		for _, deviceRef := range req.Devices {
			device, err := m.deviceManager.GetDevice(ctx, deviceRef)
			if err != nil {
				log.ErrorContext(ctx, "failed to get device", "device", deviceRef, "error", err)
				return nil, fmt.Errorf("device %s: %w", deviceRef, err)
			}
			if device.AttachedTo != nil {
				log.ErrorContext(ctx, "device already attached", "device", deviceRef, "instance", *device.AttachedTo)
				return nil, fmt.Errorf("device %s is already attached to instance %s", deviceRef, *device.AttachedTo)
			}
			// Auto-bind to VFIO if not already bound
			if !device.BoundToVFIO {
				log.InfoContext(ctx, "auto-binding device to VFIO", "device", deviceRef, "pci_address", device.PCIAddress)
				if err := m.deviceManager.BindToVFIO(ctx, device.Id); err != nil {
					log.ErrorContext(ctx, "failed to bind device to VFIO", "device", deviceRef, "error", err)
					return nil, fmt.Errorf("bind device %s to VFIO: %w", deviceRef, err)
				}
			}
			// Mark device as attached to this instance
			if err := m.deviceManager.MarkAttached(ctx, device.Id, id); err != nil {
				log.ErrorContext(ctx, "failed to mark device as attached", "device", deviceRef, "error", err)
				return nil, fmt.Errorf("mark device %s as attached: %w", deviceRef, err)
			}
			attachedDeviceIDs = append(attachedDeviceIDs, device.Id)
			resolvedDeviceIDs = append(resolvedDeviceIDs, device.Id)
		}
		log.DebugContext(ctx, "validated devices for passthrough", "id", id, "devices", resolvedDeviceIDs)
	}

	// 11. Create instance metadata
	createdAt := m.nowUTC()
	expiresAt, err := resolveCreateExpiration(req, createdAt)
	if err != nil {
		return nil, err
	}
	stored := &StoredMetadata{
		Id:                       id,
		Name:                     req.Name,
		Image:                    req.Image,
		ResolvedImage:            resolvedImageRef,
		Platform:                 imageInfo.Platform,
		Size:                     size,
		HotplugSize:              hotplugSize,
		OverlaySize:              overlaySize,
		Vcpus:                    vcpus,
		NetworkBandwidthDownload: req.NetworkBandwidthDownload, // Will be set by caller if using resource manager
		NetworkBandwidthUpload:   req.NetworkBandwidthUpload,   // Will be set by caller if using resource manager
		DiskIOBps:                req.DiskIOBps,                // Will be set by caller if using resource manager
		Env:                      req.Env,
		Tags:                     tags.Clone(req.Tags),
		NetworkEnabled:           req.NetworkEnabled,
		NetworkEgress:            cloneNetworkEgressPolicy(req.NetworkEgress),
		Credentials:              cloneCredentialPolicies(req.Credentials),
		CreatedAt:                createdAt,
		ExpiresAt:                expiresAt,
		StartedAt:                nil,
		StoppedAt:                nil,
		ProgramStartedAt:         nil,
		GuestAgentReadyAt:        nil,
		KernelVersion:            string(kernelVer),
		HypervisorType:           hvType,
		HypervisorVersion:        hvVersion,
		SocketPath:               m.paths.InstanceSocket(id, starter.SocketName()),
		DataDir:                  m.paths.InstanceDir(id),
		VsockCID:                 vsockCID,
		VsockSocket:              vsockSocket,
		Devices:                  resolvedDeviceIDs,
		GPUProfile:               gpuProfile,
		GPUFramework:             gpuFramework,
		GPUDevicePath:            gpuDevicePath,
		GPUMdevUUID:              gpuMdevUUID,
		Entrypoint:               req.Entrypoint,
		Cmd:                      req.Cmd,
		SkipKernelHeaders:        req.SkipKernelHeaders,
		SkipGuestAgent:           req.SkipGuestAgent,
		EnableRosetta:            enableRosetta,
		SnapshotPolicy:           cloneSnapshotPolicy(req.SnapshotPolicy),
		AutoStandby:              cloneAutoStandbyPolicy(req.AutoStandby),
		HealthCheck:              cloneHealthCheckPolicy(req.HealthCheck),
		RestartPolicy:            cloneRestartPolicy(req.RestartPolicy),
	}

	// 12. Ensure directories
	log.DebugContext(ctx, "creating instance directories", "instance_id", id)
	if err := m.ensureDirectories(id); err != nil {
		log.ErrorContext(ctx, "failed to create directories", "instance_id", id, "error", err)
		return nil, fmt.Errorf("ensure directories: %w", err)
	}

	// 13. Create overlay disk with specified size
	log.DebugContext(ctx, "creating overlay disk", "instance_id", id, "size_bytes", stored.OverlaySize)
	if err := m.createOverlayDisk(id, stored.OverlaySize); err != nil {
		log.ErrorContext(ctx, "failed to create overlay disk", "instance_id", id, "error", err)
		return nil, fmt.Errorf("create overlay disk: %w", err)
	}

	// 14. Allocate network (if network enabled)
	var netConfig *network.NetworkConfig
	if networkName != "" {
		log.DebugContext(ctx, "allocating network", "instance_id", id, "network", networkName,
			"download_bps", stored.NetworkBandwidthDownload, "upload_bps", stored.NetworkBandwidthUpload)
		networkCtx, networkSpanEnd := m.startLifecycleStep(ctx, "allocate_network",
			attribute.String("instance_id", id),
			attribute.String("hypervisor", string(stored.HypervisorType)),
			attribute.String("operation", "allocate_network"),
			attribute.Bool("network_enabled", true),
		)
		netConfig, err = m.networkManager.CreateAllocation(networkCtx, network.AllocateRequest{
			InstanceID:    id,
			InstanceName:  req.Name,
			DownloadBps:   stored.NetworkBandwidthDownload,
			UploadBps:     stored.NetworkBandwidthUpload,
			UploadCeilBps: stored.NetworkBandwidthUpload * int64(m.networkManager.GetUploadBurstMultiplier()),
		})
		networkSpanEnd(err)
		if err != nil {
			log.ErrorContext(ctx, "failed to allocate network", "instance_id", id, "network", networkName, "error", err)
			return nil, fmt.Errorf("allocate network: %w", err)
		}
		// Store IP/MAC in metadata (persisted with instance)
		stored.IP = netConfig.IP
		stored.MAC = netConfig.MAC
		// Add network cleanup to stack
		cu.Add(func() {
			// Network cleanup: TAP devices are removed when ReleaseAllocation is called.
			// In case of unexpected scenarios (like power loss), TAP devices persist until host reboot.
			// CreateAllocation just succeeded so the TAP exists on the host. If
			// GetAllocation can't derive a full allocation here, fall back to ID-based
			// release rather than silently leaking the TAP.
			netAlloc, err := m.networkManager.GetAllocation(ctx, id)
			if err == nil && netAlloc != nil {
				m.networkManager.ReleaseAllocation(ctx, netAlloc)
				return
			}
			m.networkManager.ReleaseByInstanceID(ctx, id)
		})
	}

	// 15. Validate and attach volumes
	if len(req.Volumes) > 0 {
		log.DebugContext(ctx, "validating volumes", "instance_id", id, "count", len(req.Volumes))
		for _, volAttach := range req.Volumes {
			// Check volume exists
			_, err := m.volumeManager.GetVolume(ctx, volAttach.VolumeID)
			if err != nil {
				log.ErrorContext(ctx, "volume not found", "instance_id", id, "volume_id", volAttach.VolumeID, "error", err)
				return nil, fmt.Errorf("volume %s: %w", volAttach.VolumeID, err)
			}

			// Mark volume as attached (AttachVolume handles multi-attach validation)
			if err := m.volumeManager.AttachVolume(ctx, volAttach.VolumeID, volumes.AttachVolumeRequest{
				InstanceID: id,
				MountPath:  volAttach.MountPath,
				Readonly:   volAttach.Readonly,
			}); err != nil {
				log.ErrorContext(ctx, "failed to attach volume", "instance_id", id, "volume_id", volAttach.VolumeID, "error", err)
				return nil, fmt.Errorf("attach volume %s: %w", volAttach.VolumeID, err)
			}

			// Add volume cleanup to stack
			volumeID := volAttach.VolumeID // capture for closure
			cu.Add(func() {
				m.volumeManager.DetachVolume(ctx, volumeID, id)
			})

			// Create overlay disk for volumes with overlay enabled
			if volAttach.Overlay {
				log.DebugContext(ctx, "creating volume overlay disk", "instance_id", id, "volume_id", volAttach.VolumeID, "size", volAttach.OverlaySize)
				if err := m.createVolumeOverlayDisk(id, volAttach.VolumeID, volAttach.OverlaySize); err != nil {
					log.ErrorContext(ctx, "failed to create volume overlay disk", "instance_id", id, "volume_id", volAttach.VolumeID, "error", err)
					return nil, fmt.Errorf("create volume overlay disk %s: %w", volAttach.VolumeID, err)
				}
			}
		}
		// Store volume attachments in metadata
		stored.Volumes = req.Volumes
	}

	// 16. Create config disk (needs Instance for buildVMConfig)
	inst := &Instance{StoredMetadata: *stored}
	var proxyGuestConfig *egressproxy.GuestConfig
	proxyGuestConfig, err = m.maybeRegisterEgressProxy(ctx, stored, netConfig)
	if err != nil {
		log.ErrorContext(ctx, "failed to configure egress proxy", "instance_id", id, "error", err)
		return nil, fmt.Errorf("configure egress proxy: %w", err)
	}
	if proxyGuestConfig != nil {
		cu.Add(func() {
			m.unregisterEgressProxyInstance(ctx, id)
		})
	}
	log.DebugContext(ctx, "creating config disk", "instance_id", id)
	configDiskCtx, configDiskSpanEnd := m.startLifecycleStep(ctx, "create_config_disk",
		attribute.String("instance_id", id),
		attribute.String("hypervisor", string(stored.HypervisorType)),
		attribute.String("operation", "create_config_disk"),
	)
	if err := m.createConfigDisk(configDiskCtx, inst, imageInfo, netConfig, proxyGuestConfig); err != nil {
		configDiskSpanEnd(err)
		log.ErrorContext(ctx, "failed to create config disk", "instance_id", id, "error", err)
		return nil, fmt.Errorf("create config disk: %w", err)
	}
	configDiskSpanEnd(nil)

	// 17. Record boot start time before launching the VM so marker hydration
	// can safely ignore stale sentinels from prior runs.
	if err := m.archiveAppLogForBoot(id); err != nil {
		log.WarnContext(ctx, "failed to archive app log before create boot", "instance_id", id, "error", err)
	}
	bootStart := time.Now().UTC()
	stored.StartedAt = &bootStart
	stored.Phases.Record(phasetracking.PhaseCreated, bootStart)

	// 18. Save metadata
	log.DebugContext(ctx, "saving instance metadata", "instance_id", id)
	meta := &metadata{StoredMetadata: *stored}
	if err := m.saveMetadata(meta); err != nil {
		log.ErrorContext(ctx, "failed to save metadata", "instance_id", id, "error", err)
		return nil, fmt.Errorf("save metadata: %w", err)
	}

	// 19. Start VMM and boot VM
	log.InfoContext(ctx, "starting VMM and booting VM", "instance_id", id, "hypervisor", hvType, "version", hvVersion)
	startVMCtx, startVMSpanEnd := m.startLifecycleStep(ctx, "start_vm",
		attribute.String("instance_id", id),
		attribute.String("hypervisor", string(stored.HypervisorType)),
		attribute.String("operation", "start_vm"),
	)
	if err := m.startAndBootVM(startVMCtx, stored, imageInfo, netConfig, false); err != nil {
		startVMSpanEnd(err)
		log.ErrorContext(ctx, "failed to start and boot VM", "instance_id", id, "error", err)
		return nil, err
	}
	startVMSpanEnd(nil)
	// Mark the instance visible before releasing its pending reservation so we
	// never create an undercount window. The tiny overlap is intentionally
	// over-conservative: concurrent admissions may briefly see both visible and
	// pending usage for this instance, but they will not oversubscribe the host.
	m.setAdmissionAllocationActive(stored, true)
	if reservedResources {
		m.resourceValidator.FinishAllocation(id)
		reservedResources = false
	}

	// 20. Persist runtime metadata updates after VM boot. The VMM is up but
	// guest boot markers have not yet been written, so we are in Initializing;
	// persistBootMarkers will advance us to Running once the markers appear
	// in the serial log.
	stored.Phases.Record(phasetracking.PhaseInitializing, time.Now().UTC())
	meta = &metadata{StoredMetadata: *stored}
	if err := m.saveMetadata(meta); err != nil {
		// VM is running but metadata failed - log but don't fail
		// Instance is recoverable, state will be derived
		log.WarnContext(ctx, "failed to update metadata after VM start", "instance_id", id, "error", err)
	}

	// Success - release cleanup stack (prevent cleanup)
	cu.Release()

	// Return instance state from current metadata without forcing a log scan.
	finalInst := m.toInstanceWithoutHydration(ctx, meta)
	// Record metrics
	if m.metrics != nil {
		m.recordDuration(ctx, m.metrics.createDuration, start, "success", hvType)
		m.recordStateTransition(ctx, string(StateStopped), string(finalInst.State), hvType)
	}
	log.InfoContext(ctx, "instance created successfully", "instance_id", id, "name", req.Name, "state", finalInst.State, "hypervisor", hvType, "version", hvVersion)
	return &finalInst, nil
}

func resolveCreateExpiration(req CreateInstanceRequest, now time.Time) (*time.Time, error) {
	if req.ExpiresAt != nil {
		if err := validateExpiresAt(req.ExpiresAt, now); err != nil {
			return nil, err
		}
		expiresAt := req.ExpiresAt.UTC()
		return &expiresAt, nil
	}
	if req.TTL == nil || *req.TTL == 0 {
		return nil, nil
	}
	expiresAt := now.Add(*req.TTL)
	return &expiresAt, nil
}

// validateCreateRequest validates the create instance request.
// The request is mutated in-place to persist normalized egress/credential policy fields.
func validateCreateRequest(req *CreateInstanceRequest) error {
	if req == nil {
		return fmt.Errorf("%w: request is required", ErrInvalidRequest)
	}
	if err := validateInstanceName(req.Name); err != nil {
		return err
	}
	if req.Image == "" {
		return fmt.Errorf("image is required")
	}
	if req.Size < 0 {
		return fmt.Errorf("size cannot be negative")
	}
	if req.HotplugSize < 0 {
		return fmt.Errorf("hotplug_size cannot be negative")
	}
	if req.OverlaySize < 0 {
		return fmt.Errorf("overlay_size cannot be negative")
	}
	if req.Vcpus < 0 {
		return fmt.Errorf("vcpus cannot be negative")
	}
	if req.TTL != nil && *req.TTL < 0 {
		return fmt.Errorf("%w: ttl cannot be negative", ErrInvalidRequest)
	}
	if req.TTL != nil && req.ExpiresAt != nil {
		return fmt.Errorf("%w: ttl and expires_at are mutually exclusive", ErrInvalidRequest)
	}
	if req.NetworkEgress != nil && req.NetworkEgress.Enabled {
		if !req.NetworkEnabled {
			return fmt.Errorf("%w: network.egress requires network.enabled=true", ErrInvalidRequest)
		}
		mode, err := normalizeEgressEnforcementMode(req.NetworkEgress.EnforcementMode)
		if err != nil {
			return err
		}
		req.NetworkEgress.EnforcementMode = mode
	}
	normalizedCredentials, err := normalizeCredentialPolicies(req.Credentials)
	if err != nil {
		return err
	}
	req.Credentials = normalizedCredentials
	if len(normalizedCredentials) > 0 {
		if req.NetworkEgress == nil || !req.NetworkEgress.Enabled {
			return fmt.Errorf("%w: credentials require network.egress.enabled=true", ErrInvalidRequest)
		}
		if err := validateCredentialEnvBindings(normalizedCredentials, req.Env); err != nil {
			return err
		}
	}
	if err := tags.Validate(req.Tags); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidRequest, err)
	}
	if req.SnapshotPolicy != nil && req.SnapshotPolicy.Compression != nil {
		if _, err := normalizeCompressionConfig(req.SnapshotPolicy.Compression); err != nil {
			return err
		}
	}
	if req.SnapshotPolicy != nil && req.SnapshotPolicy.StandbyCompressionDelay != nil {
		if _, err := normalizeStandbyCompressionDelay(req.SnapshotPolicy.StandbyCompressionDelay); err != nil {
			return err
		}
	}
	normalizedAutoStandby, err := normalizeAutoStandbyPolicy(req.AutoStandby)
	if err != nil {
		return err
	}
	req.AutoStandby = normalizedAutoStandby
	normalizedHealthCheck, err := normalizeHealthCheckPolicy(req.HealthCheck)
	if err != nil {
		return err
	}
	req.HealthCheck = normalizedHealthCheck
	if err := validateHealthCheckCompatibility(req.HealthCheck, req.NetworkEnabled, req.SkipGuestAgent); err != nil {
		return err
	}
	normalizedRestartPolicy, err := normalizeRestartPolicy(req.RestartPolicy)
	if err != nil {
		return err
	}
	req.RestartPolicy = normalizedRestartPolicy

	// Validate volume attachments
	if err := validateVolumeAttachmentsWithSystemPaths(req.Volumes, req.AllowSystemVolumeMounts, req.SystemVolumeMountPaths); err != nil {
		return err
	}

	return nil
}

// validateVolumeAttachments validates public volume attachment requests.
func validateVolumeAttachments(attachments []VolumeAttachment, allowSystemVolumes bool) error {
	return validateVolumeAttachmentsWithSystemPaths(attachments, allowSystemVolumes, nil)
}

// validateVolumeAttachmentsWithSystemPaths validates volume attachments for
// internal callers that may attach reserved volumes at explicitly allowed
// system paths.
func validateVolumeAttachmentsWithSystemPaths(attachments []VolumeAttachment, allowSystemVolumes bool, allowedSystemMountPaths []string) error {
	// Count total devices needed (each overlay volume needs 2 devices: base + overlay)
	totalDevices := 0
	for _, vol := range attachments {
		totalDevices++
		if vol.Overlay {
			totalDevices++ // Overlay needs an additional device
		}
	}
	if totalDevices > MaxVolumesPerInstance {
		return fmt.Errorf("cannot attach more than %d volume devices per instance (overlay volumes count as 2)", MaxVolumesPerInstance)
	}

	seenPaths := make(map[string]bool)
	for _, vol := range attachments {
		// Validate mount path is absolute
		if !filepath.IsAbs(vol.MountPath) {
			return fmt.Errorf("volume %s: mount path %q must be absolute", vol.VolumeID, vol.MountPath)
		}

		// Clean the path to normalize it
		cleanPath := filepath.Clean(vol.MountPath)

		// Check for system directories
		if isSystemDirectory(cleanPath) && !(allowSystemVolumes && isAllowedSystemMountPath(cleanPath, allowedSystemMountPaths)) {
			return fmt.Errorf("volume %s: cannot mount to system directory %q", vol.VolumeID, cleanPath)
		}

		// Reserved internal volume IDs are attachable only by internal instances
		if !allowSystemVolumes {
			if prefix := volumes.ReservedVolumeIDPrefix(vol.VolumeID); prefix != "" {
				return fmt.Errorf("volume %s: volume IDs with the prefix %q are reserved for internal use", vol.VolumeID, prefix)
			}
		}

		// Check for duplicate mount paths
		if seenPaths[cleanPath] {
			return fmt.Errorf("duplicate mount path %q", cleanPath)
		}
		seenPaths[cleanPath] = true

		// Validate overlay mode requirements
		if vol.Overlay {
			if !vol.Readonly {
				return fmt.Errorf("volume %s: overlay mode requires readonly=true", vol.VolumeID)
			}
			if vol.OverlaySize <= 0 {
				return fmt.Errorf("volume %s: overlay_size is required when overlay=true", vol.VolumeID)
			}
		}
	}

	return nil
}

// isAllowedSystemMountPath reports whether path is an exact match for a
// system path that an internal instance may mount a volume at.
func isAllowedSystemMountPath(path string, allowedSystemMountPaths []string) bool {
	for _, allowed := range allowedSystemMountPaths {
		if path == allowed {
			return true
		}
	}
	return false
}

// isSystemDirectory checks if a path is or is under a system directory
func isSystemDirectory(path string) bool {
	cleanPath := filepath.Clean(path)
	for _, sysDir := range systemDirectories {
		if cleanPath == sysDir {
			return true
		}
		// Also block subdirectories of system paths (except / which would block everything)
		if sysDir != "/" && (strings.HasPrefix(cleanPath, sysDir+"/") || cleanPath == sysDir) {
			return true
		}
	}
	return false
}

// startAndBootVM starts the VMM and boots the VM
func (m *manager) startAndBootVM(
	ctx context.Context,
	stored *StoredMetadata,
	imageInfo *images.Image,
	netConfig *network.NetworkConfig,
	refreshHostVersion bool,
) error {
	log := logger.FromContext(ctx)

	// Get VM starter for this hypervisor type
	starter, err := m.getVMStarter(stored.HypervisorType)
	if err != nil {
		return fmt.Errorf("get vm starter: %w", err)
	}

	// Version-locked snapshot backends must refresh metadata on every cold
	// start so host upgrades do not leave the reported version stale.
	if refreshHostVersion && requiresHostSnapshotVersion(stored.HypervisorType) {
		resolvedVersion, err := starter.ResolveVersion(m.paths, "")
		if err != nil {
			return fmt.Errorf("resolve version for hypervisor %s start: %w", stored.HypervisorType, err)
		}
		stored.HypervisorVersion = resolvedVersion
	}

	// Build VM configuration
	inst := &Instance{StoredMetadata: *stored}
	vmConfig, err := m.buildHypervisorConfig(ctx, inst, imageInfo, netConfig)
	if err != nil {
		return fmt.Errorf("build vm config: %w", err)
	}

	// Start VM (handles process start, configuration, and boot)
	log.DebugContext(ctx, "starting VM", "instance_id", stored.Id, "hypervisor", stored.HypervisorType, "version", stored.HypervisorVersion)
	pid, hv, err := starter.StartVM(ctx, m.paths, stored.HypervisorVersion, stored.SocketPath, vmConfig)
	if err != nil {
		return fmt.Errorf("start vm: %w", err)
	}
	pid = resolveRuntimeHypervisorPID(log, stored.SocketPath, pid)

	// Store the PID for later cleanup
	stored.HypervisorPID = &pid
	log.DebugContext(ctx, "VM started", "instance_id", stored.Id, "pid", pid)

	// Optional: Expand memory to max if hotplug configured
	if inst.HotplugSize > 0 && hv.Capabilities().SupportsHotplugMemory {
		totalBytes := inst.Size + inst.HotplugSize
		log.DebugContext(ctx, "expanding VM memory", "instance_id", stored.Id, "total_bytes", totalBytes)
		// Best effort, ignore errors
		if err := hv.ResizeMemory(ctx, totalBytes); err != nil {
			log.WarnContext(ctx, "failed to expand VM memory", "instance_id", stored.Id, "error", err)
		}
	}

	return nil
}

func resolveRuntimeHypervisorPID(log *slog.Logger, socketPath string, fallbackPID int) int {
	if ProcessExists(fallbackPID) {
		return fallbackPID
	}
	pid, err := hypervisor.ResolveProcessPID(socketPath)
	if err != nil {
		log.Debug("using fallback hypervisor pid", "socket_path", socketPath, "pid", fallbackPID, "error", err)
		return fallbackPID
	}
	return pid
}

// buildHypervisorConfig creates a hypervisor-agnostic VM configuration
func (m *manager) buildHypervisorConfig(ctx context.Context, inst *Instance, imageInfo *images.Image, netConfig *network.NetworkConfig) (hypervisor.VMConfig, error) {
	// Get system file paths
	kernelPath, _ := m.systemManager.GetKernelPath(system.KernelVersion(inst.KernelVersion))
	initrdPath, _ := m.systemManager.GetInitrdPath()

	// Disk configuration
	// Get rootfs disk path from image manager
	rootfsPath, err := images.GetDiskPath(m.paths, imageInfo.Name, imageInfo.Digest)
	if err != nil {
		return hypervisor.VMConfig{}, err
	}

	// Get disk I/O limits (same for all disks in this VM)
	ioBps := inst.DiskIOBps
	burstBps := ioBps * 4 // Burst is 4x sustained
	if ioBps <= 0 {
		burstBps = 0
	}

	disks := make([]hypervisor.DiskConfig, 0, instanceDiskCount(inst.Volumes))
	disks = append(disks,
		// Rootfs (from image, read-only)
		hypervisor.DiskConfig{Path: rootfsPath, Readonly: true, IOBps: ioBps, IOBurstBps: burstBps},
		// Overlay disk (writable)
		hypervisor.DiskConfig{Path: m.paths.InstanceOverlay(inst.Id), Readonly: false, IOBps: ioBps, IOBurstBps: burstBps},
		// Config disk (read-only)
		hypervisor.DiskConfig{Path: m.paths.InstanceConfigDisk(inst.Id), Readonly: true, IOBps: ioBps, IOBurstBps: burstBps},
	)

	// Add attached volumes as additional disks
	for _, volAttach := range inst.Volumes {
		volumePath, err := m.volumeManager.GetVolumePath(volAttach.VolumeID)
		if err != nil {
			return hypervisor.VMConfig{}, fmt.Errorf("get volume path: %w", err)
		}
		if volAttach.Overlay {
			// Base volume is always read-only when overlay is enabled
			disks = append(disks, hypervisor.DiskConfig{
				Path:       volumePath,
				Readonly:   true,
				IOBps:      ioBps,
				IOBurstBps: burstBps,
			})
			// Overlay disk is writable
			overlayPath := m.paths.InstanceVolumeOverlay(inst.Id, volAttach.VolumeID)
			disks = append(disks, hypervisor.DiskConfig{
				Path:       overlayPath,
				Readonly:   false,
				IOBps:      ioBps,
				IOBurstBps: burstBps,
			})
		} else {
			disks = append(disks, hypervisor.DiskConfig{
				Path:       volumePath,
				Readonly:   volAttach.Readonly,
				IOBps:      ioBps,
				IOBurstBps: burstBps,
			})
		}
	}

	if len(disks) != instanceDiskCount(inst.Volumes) {
		return hypervisor.VMConfig{}, fmt.Errorf("internal disk plan mismatch: built %d disks, planned %d", len(disks), instanceDiskCount(inst.Volumes))
	}

	// Network configuration
	var networks []hypervisor.NetworkConfig
	if netConfig != nil {
		// Instance-level bandwidth limits are persisted in metadata, then passed
		// into per-interface hypervisor config so VMMs like Firecracker can map
		// them to device-level API rate limiters.
		networks = append(networks, hypervisor.NetworkConfig{
			TAPDevice:   netConfig.TAPDevice,
			IP:          netConfig.IP,
			MAC:         netConfig.MAC,
			Netmask:     netConfig.Netmask,
			DownloadBps: inst.NetworkBandwidthDownload,
			UploadBps:   inst.NetworkBandwidthUpload,
		})
	}

	// Device passthrough configuration (GPU, etc.)
	var pciDevices []string
	if len(inst.Devices) > 0 && m.deviceManager != nil {
		for _, deviceID := range inst.Devices {
			device, err := m.deviceManager.GetDevice(ctx, deviceID)
			if err != nil {
				return hypervisor.VMConfig{}, fmt.Errorf("get device %s: %w", deviceID, err)
			}
			pciDevices = append(pciDevices, devices.GetDeviceSysfsPath(device.PCIAddress))
		}
	}

	// Build topology if available
	var topology *hypervisor.CPUTopology
	if hostTopo := calculateGuestTopology(inst.Vcpus, m.hostTopology); hostTopo != nil {
		topology = &hypervisor.CPUTopology{}
		if hostTopo.ThreadsPerCore != nil {
			topology.ThreadsPerCore = *hostTopo.ThreadsPerCore
		}
		if hostTopo.CoresPerDie != nil {
			topology.CoresPerDie = *hostTopo.CoresPerDie
		}
		if hostTopo.DiesPerPackage != nil {
			topology.DiesPerPackage = *hostTopo.DiesPerPackage
		}
		if hostTopo.Packages != nil {
			topology.Packages = *hostTopo.Packages
		}
	}

	return hypervisor.VMConfig{
		VCPUs:          inst.Vcpus,
		MemoryBytes:    inst.Size,
		HotplugBytes:   inst.HotplugSize,
		Topology:       topology,
		GuestMemory:    m.guestMemoryConfig(),
		Disks:          disks,
		Networks:       networks,
		SerialLogPath:  m.paths.InstanceAppLog(inst.Id),
		VsockCID:       inst.VsockCID,
		VsockSocket:    inst.VsockSocket,
		PCIDevices:     pciDevices,
		VGPUDevicePath: storedVGPUDevicePath(&inst.StoredMetadata),
		KernelPath:     kernelPath,
		InitrdPath:     initrdPath,
		KernelArgs:     m.kernelArgs(inst.HypervisorType),
		EnableRosetta:  inst.EnableRosetta,
	}, nil
}

func resolveCreateKernelVersion(imageInfo *images.Image, defaultKernel system.KernelVersion) (system.KernelVersion, error) {
	if imageInfo == nil || len(imageInfo.Labels) == 0 {
		return defaultKernel, nil
	}

	requested := strings.TrimSpace(imageInfo.Labels[system.ImageKernelVersionLabel])
	if requested == "" {
		return defaultKernel, nil
	}

	kernelVer, ok := system.ParseKernelVersion(requested)
	if !ok {
		return "", fmt.Errorf("%w: image %s requests unsupported kernel version %q via label %s",
			ErrInvalidRequest, imageInfo.Name, requested, system.ImageKernelVersionLabel)
	}

	return kernelVer, nil
}

// kernelArgs returns the kernel command line arguments for the given hypervisor type.
// vz uses hvc0 (virtio console), all others use ttyS0 (serial port).
func (m *manager) kernelArgs(hvType hypervisor.Type) string {
	console := "console=ttyS0"
	if hvType == hypervisor.TypeVZ {
		console = "console=hvc0"
	}
	policyArgs := strings.Join(m.guestMemoryPolicy.KernelArgs(), " ")
	return guestmemory.MergeKernelArgs(console, policyArgs)
}

func (m *manager) guestMemoryConfig() hypervisor.GuestMemoryConfig {
	features := m.guestMemoryPolicy.FeaturesForHypervisor()
	return hypervisor.GuestMemoryConfig{
		EnableBalloon:     features.EnableBalloon,
		FreePageReporting: features.FreePageReporting,
		DeflateOnOOM:      features.DeflateOnOOM,
		FreePageHinting:   features.FreePageHinting,
		RequireBalloon:    features.RequireBalloon,
	}
}

func ptr[T any](v T) *T {
	return &v
}
