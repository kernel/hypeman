package instances

import (
	"context"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/nrednav/cuid2"
	"github.com/onkernel/hypeman/lib/devices"
	"github.com/onkernel/hypeman/lib/hypervisor"
	"github.com/onkernel/hypeman/lib/images"
	"github.com/onkernel/hypeman/lib/logger"
	"github.com/onkernel/hypeman/lib/network"
	"github.com/onkernel/hypeman/lib/system"
	"github.com/onkernel/hypeman/lib/volumes"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
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

// AggregateUsage represents total resource usage across all instances
type AggregateUsage struct {
	TotalVcpus  int
	TotalMemory int64 // in bytes
}

// calculateAggregateUsage calculates total resource usage across all running instances
func (m *manager) calculateAggregateUsage(ctx context.Context) (AggregateUsage, error) {
	instances, err := m.listInstances(ctx)
	if err != nil {
		return AggregateUsage{}, err
	}

	var usage AggregateUsage
	for _, inst := range instances {
		// Only count running/paused instances (those consuming resources)
		if inst.State == StateRunning || inst.State == StatePaused || inst.State == StateCreated {
			usage.TotalVcpus += inst.Vcpus
			usage.TotalMemory += inst.Size + inst.HotplugSize
		}
	}

	return usage, nil
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
) (*Instance, error) {
	start := time.Now()
	log := logger.FromContext(ctx)
	log.InfoContext(ctx, "creating instance", "name", req.Name, "image", req.Image, "vcpus", req.Vcpus)

	// Start tracing span if tracer is available
	if m.metrics != nil && m.metrics.tracer != nil {
		var span trace.Span
		ctx, span = m.metrics.tracer.Start(ctx, "CreateInstance")
		defer span.End()
	}

	// 1. Validate request
	if err := validateCreateRequest(req); err != nil {
		log.ErrorContext(ctx, "invalid create request", "error", err)
		return nil, err
	}

	// 2. Validate image exists and is ready
	log.DebugContext(ctx, "validating image", "image", req.Image)
	imageInfo, err := m.imageManager.GetImage(ctx, req.Image)
	if err != nil {
		log.ErrorContext(ctx, "failed to get image", "image", req.Image, "error", err)
		if err == images.ErrNotFound {
			return nil, fmt.Errorf("image %s: %w", req.Image, err)
		}
		return nil, fmt.Errorf("get image: %w", err)
	}

	if imageInfo.Status != images.StatusReady {
		log.ErrorContext(ctx, "image not ready", "image", req.Image, "status", imageInfo.Status)
		return nil, fmt.Errorf("%w: image status is %s", ErrImageNotReady, imageInfo.Status)
	}

	// 3. Generate instance ID (CUID2 for secure, collision-resistant IDs)
	id := cuid2.Generate()
	log.DebugContext(ctx, "generated instance ID", "instance_id", id)

	// 4. Generate vsock configuration
	vsockCID := generateVsockCID(id)
	vsockSocket := m.paths.InstanceVsockSocket(id)
	log.DebugContext(ctx, "generated vsock config", "instance_id", id, "cid", vsockCID)

	// 5. Check instance doesn't already exist
	if _, err := m.loadMetadata(id); err == nil {
		return nil, ErrAlreadyExists
	}

	// 6. Apply defaults
	size := req.Size
	if size == 0 {
		size = 1 * 1024 * 1024 * 1024 // 1GB default
	}
	hotplugSize := req.HotplugSize
	if hotplugSize == 0 {
		hotplugSize = 3 * 1024 * 1024 * 1024 // 3GB default
	}
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

	// Validate aggregate resource limits
	if m.limits.MaxTotalVcpus > 0 || m.limits.MaxTotalMemory > 0 {
		usage, err := m.calculateAggregateUsage(ctx)
		if err != nil {
			log.WarnContext(ctx, "failed to calculate aggregate usage, skipping limit check", "error", err)
		} else {
			if m.limits.MaxTotalVcpus > 0 && usage.TotalVcpus+vcpus > m.limits.MaxTotalVcpus {
				return nil, fmt.Errorf("total vcpus would be %d, exceeds aggregate limit of %d", usage.TotalVcpus+vcpus, m.limits.MaxTotalVcpus)
			}
			if m.limits.MaxTotalMemory > 0 && usage.TotalMemory+totalMemory > m.limits.MaxTotalMemory {
				return nil, fmt.Errorf("total memory would be %d, exceeds aggregate limit of %d", usage.TotalMemory+totalMemory, m.limits.MaxTotalMemory)
			}
		}
	}

	if req.Env == nil {
		req.Env = make(map[string]string)
	}

	// 7. Determine network based on NetworkEnabled flag
	networkName := ""
	if req.NetworkEnabled {
		networkName = "default"
	}

	// 8. Get default kernel version
	kernelVer := m.systemManager.GetDefaultKernelVersion()

	// 9. Get process manager for hypervisor type (needed for socket name)
	hvType := req.Hypervisor
	if hvType == "" {
		hvType = m.defaultHypervisor
	}

	// Enrich logger and trace span with hypervisor type
	log = log.With("hypervisor", string(hvType))
	ctx = logger.AddToContext(ctx, log)
	if m.metrics != nil && m.metrics.tracer != nil {
		span := trace.SpanFromContext(ctx)
		if span.IsRecording() {
			span.SetAttributes(attribute.String("hypervisor", string(hvType)))
		}
	}

	starter, err := m.getVMStarter(hvType)
	if err != nil {
		log.ErrorContext(ctx, "failed to get vm starter", "error", err)
		return nil, fmt.Errorf("get vm starter for %s: %w", hvType, err)
	}

	// Get hypervisor version
	hvVersion, err := starter.GetVersion(m.paths)
	if err != nil {
		log.WarnContext(ctx, "failed to get hypervisor version", "hypervisor", hvType, "error", err)
		hvVersion = "unknown"
	}

	// 10. Validate, resolve, and auto-bind devices (GPU passthrough)
	// Track devices we've marked as attached for cleanup on error.
	// The cleanup closure captures this slice by reference, so it will see
	// whatever devices have been attached when cleanup runs.
	var attachedDeviceIDs []string
	var resolvedDeviceIDs []string

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
	stored := &StoredMetadata{
		Id:                id,
		Name:              req.Name,
		Image:             req.Image,
		Size:              size,
		HotplugSize:       hotplugSize,
		OverlaySize:       overlaySize,
		Vcpus:             vcpus,
		Env:               req.Env,
		NetworkEnabled:    req.NetworkEnabled,
		CreatedAt:         time.Now(),
		StartedAt:         nil,
		StoppedAt:         nil,
		KernelVersion:     string(kernelVer),
		HypervisorType:    hvType,
		HypervisorVersion: hvVersion,
		SocketPath:        m.paths.InstanceSocket(id, starter.SocketName()),
		DataDir:           m.paths.InstanceDir(id),
		VsockCID:          vsockCID,
		VsockSocket:       vsockSocket,
		Devices:           resolvedDeviceIDs,
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
		log.DebugContext(ctx, "allocating network", "instance_id", id, "network", networkName)
		netConfig, err = m.networkManager.CreateAllocation(ctx, network.AllocateRequest{
			InstanceID:   id,
			InstanceName: req.Name,
		})
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
			if netAlloc, err := m.networkManager.GetAllocation(ctx, id); err == nil {
				m.networkManager.ReleaseAllocation(ctx, netAlloc)
			}
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
	log.DebugContext(ctx, "creating config disk", "instance_id", id)
	if err := m.createConfigDisk(ctx, inst, imageInfo, netConfig); err != nil {
		log.ErrorContext(ctx, "failed to create config disk", "instance_id", id, "error", err)
		return nil, fmt.Errorf("create config disk: %w", err)
	}

	// 17. Save metadata
	log.DebugContext(ctx, "saving instance metadata", "instance_id", id)
	meta := &metadata{StoredMetadata: *stored}
	if err := m.saveMetadata(meta); err != nil {
		log.ErrorContext(ctx, "failed to save metadata", "instance_id", id, "error", err)
		return nil, fmt.Errorf("save metadata: %w", err)
	}

	// 18. Start VMM and boot VM
	log.InfoContext(ctx, "starting VMM and booting VM", "instance_id", id)
	if err := m.startAndBootVM(ctx, stored, imageInfo, netConfig); err != nil {
		log.ErrorContext(ctx, "failed to start and boot VM", "instance_id", id, "error", err)
		return nil, err
	}

	// 19. Update timestamp after VM is running
	now := time.Now()
	stored.StartedAt = &now

	meta = &metadata{StoredMetadata: *stored}
	if err := m.saveMetadata(meta); err != nil {
		// VM is running but metadata failed - log but don't fail
		// Instance is recoverable, state will be derived
		log.WarnContext(ctx, "failed to update metadata after VM start", "instance_id", id, "error", err)
	}

	// Success - release cleanup stack (prevent cleanup)
	cu.Release()

	// Record metrics
	if m.metrics != nil {
		m.recordDuration(ctx, m.metrics.createDuration, start, "success", hvType)
		m.recordStateTransition(ctx, "stopped", string(StateRunning), hvType)
	}

	// Return instance with derived state
	finalInst := m.toInstance(ctx, meta)
	log.InfoContext(ctx, "instance created successfully", "instance_id", id, "name", req.Name, "state", finalInst.State, "hypervisor", hvType)
	return &finalInst, nil
}

// validateCreateRequest validates the create instance request
func validateCreateRequest(req CreateInstanceRequest) error {
	if req.Name == "" {
		return fmt.Errorf("name is required")
	}
	// Validate name format: lowercase letters, digits, dashes only
	// No starting/ending with dashes, max 63 characters
	if len(req.Name) > 63 {
		return fmt.Errorf("name must be 63 characters or less")
	}
	namePattern := regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)
	if !namePattern.MatchString(req.Name) {
		return fmt.Errorf("name must contain only lowercase letters, digits, and dashes; cannot start or end with a dash")
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

	// Validate volume attachments
	if err := validateVolumeAttachments(req.Volumes); err != nil {
		return err
	}

	return nil
}

// validateVolumeAttachments validates volume attachment requests
func validateVolumeAttachments(volumes []VolumeAttachment) error {
	// Count total devices needed (each overlay volume needs 2 devices: base + overlay)
	totalDevices := 0
	for _, vol := range volumes {
		totalDevices++
		if vol.Overlay {
			totalDevices++ // Overlay needs an additional device
		}
	}
	if totalDevices > MaxVolumesPerInstance {
		return fmt.Errorf("cannot attach more than %d volume devices per instance (overlay volumes count as 2)", MaxVolumesPerInstance)
	}

	seenPaths := make(map[string]bool)
	for _, vol := range volumes {
		// Validate mount path is absolute
		if !filepath.IsAbs(vol.MountPath) {
			return fmt.Errorf("volume %s: mount path %q must be absolute", vol.VolumeID, vol.MountPath)
		}

		// Clean the path to normalize it
		cleanPath := filepath.Clean(vol.MountPath)

		// Check for system directories
		if isSystemDirectory(cleanPath) {
			return fmt.Errorf("volume %s: cannot mount to system directory %q", vol.VolumeID, cleanPath)
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
) error {
	log := logger.FromContext(ctx)

	// Get VM starter for this hypervisor type
	starter, err := m.getVMStarter(stored.HypervisorType)
	if err != nil {
		return fmt.Errorf("get vm starter: %w", err)
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

	disks := []hypervisor.DiskConfig{
		// Rootfs (from image, read-only)
		{Path: rootfsPath, Readonly: true},
		// Overlay disk (writable)
		{Path: m.paths.InstanceOverlay(inst.Id), Readonly: false},
		// Config disk (read-only)
		{Path: m.paths.InstanceConfigDisk(inst.Id), Readonly: true},
	}

	// Add attached volumes as additional disks
	for _, volAttach := range inst.Volumes {
		volumePath := m.volumeManager.GetVolumePath(volAttach.VolumeID)
		if volAttach.Overlay {
			// Base volume is always read-only when overlay is enabled
			disks = append(disks, hypervisor.DiskConfig{
				Path:     volumePath,
				Readonly: true,
			})
			// Overlay disk is writable
			overlayPath := m.paths.InstanceVolumeOverlay(inst.Id, volAttach.VolumeID)
			disks = append(disks, hypervisor.DiskConfig{
				Path:     overlayPath,
				Readonly: false,
			})
		} else {
			disks = append(disks, hypervisor.DiskConfig{
				Path:     volumePath,
				Readonly: volAttach.Readonly,
			})
		}
	}

	// Network configuration
	var networks []hypervisor.NetworkConfig
	if netConfig != nil {
		networks = append(networks, hypervisor.NetworkConfig{
			TAPDevice: netConfig.TAPDevice,
			IP:        netConfig.IP,
			MAC:       netConfig.MAC,
			Netmask:   netConfig.Netmask,
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
		VCPUs:         inst.Vcpus,
		MemoryBytes:   inst.Size,
		HotplugBytes:  inst.HotplugSize,
		Topology:      topology,
		Disks:         disks,
		Networks:      networks,
		SerialLogPath: m.paths.InstanceAppLog(inst.Id),
		VsockCID:      inst.VsockCID,
		VsockSocket:   inst.VsockSocket,
		PCIDevices:    pciDevices,
		KernelPath:    kernelPath,
		InitrdPath:    initrdPath,
		KernelArgs:    "console=ttyS0",
	}, nil
}

func ptr[T any](v T) *T {
	return &v
}
