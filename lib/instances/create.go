package instances

import (
	"context"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/nrednav/cuid2"
	"github.com/onkernel/hypeman/lib/images"
	"github.com/onkernel/hypeman/lib/logger"
	"github.com/onkernel/hypeman/lib/network"
	"github.com/onkernel/hypeman/lib/system"
	"github.com/onkernel/hypeman/lib/vmm"
	"github.com/onkernel/hypeman/lib/volumes"
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
	log := logger.FromContext(ctx)
	log.InfoContext(ctx, "creating instance", "name", req.Name, "image", req.Image, "vcpus", req.Vcpus)
	
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
	log.DebugContext(ctx, "generated instance ID", "id", id)

	// 4. Generate vsock configuration
	vsockCID := generateVsockCID(id)
	vsockSocket := m.paths.InstanceVsockSocket(id)
	log.DebugContext(ctx, "generated vsock config", "id", id, "cid", vsockCID)

	// 5. Check instance doesn't already exist
	if _, err := m.loadMetadata(id); err == nil {
		return nil, ErrAlreadyExists
	}

	// 5. Apply defaults
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

	// 6. Determine network based on NetworkEnabled flag
	networkName := ""
	if req.NetworkEnabled {
		networkName = "default"
	}

	// 7. Get default kernel version
	kernelVer := m.systemManager.GetDefaultKernelVersion()

	// 8. Create instance metadata
	stored := &StoredMetadata{
		Id:             id,
		Name:           req.Name,
		Image:          req.Image,
		Size:           size,
		HotplugSize:    hotplugSize,
		OverlaySize:    overlaySize,
		Vcpus:          vcpus,
		Env:            req.Env,
		NetworkEnabled: req.NetworkEnabled,
		CreatedAt:      time.Now(),
		StartedAt:      nil,
		StoppedAt:      nil,
		KernelVersion:  string(kernelVer),
		CHVersion:      vmm.V49_0, // Use latest
		SocketPath:     m.paths.InstanceSocket(id),
		DataDir:        m.paths.InstanceDir(id),
		VsockCID:       vsockCID,
		VsockSocket:    vsockSocket,
	}

	// Setup cleanup stack for automatic rollback on errors
	cu := cleanup.Make(func() {
		log.DebugContext(ctx, "cleaning up instance on error", "id", id)
		m.deleteInstanceData(id)
	})
	defer cu.Clean()

	// 8. Ensure directories
	log.DebugContext(ctx, "creating instance directories", "id", id)
	if err := m.ensureDirectories(id); err != nil {
		log.ErrorContext(ctx, "failed to create directories", "id", id, "error", err)
		return nil, fmt.Errorf("ensure directories: %w", err)
	}

	// 9. Create overlay disk with specified size
	log.DebugContext(ctx, "creating overlay disk", "id", id, "size_bytes", stored.OverlaySize)
	if err := m.createOverlayDisk(id, stored.OverlaySize); err != nil {
		log.ErrorContext(ctx, "failed to create overlay disk", "id", id, "error", err)
		return nil, fmt.Errorf("create overlay disk: %w", err)
	}

	// 10. Allocate network (if network enabled)
	var netConfig *network.NetworkConfig
	if networkName != "" {
		log.DebugContext(ctx, "allocating network", "id", id, "network", networkName)
		netConfig, err = m.networkManager.CreateAllocation(ctx, network.AllocateRequest{
			InstanceID:   id,
			InstanceName: req.Name,
		})
		if err != nil {
			log.ErrorContext(ctx, "failed to allocate network", "id", id, "network", networkName, "error", err)
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

	// 10.5. Validate and attach volumes
	if len(req.Volumes) > 0 {
		log.DebugContext(ctx, "validating volumes", "id", id, "count", len(req.Volumes))
		for _, volAttach := range req.Volumes {
			// Check volume exists
			_, err := m.volumeManager.GetVolume(ctx, volAttach.VolumeID)
			if err != nil {
				log.ErrorContext(ctx, "volume not found", "id", id, "volume_id", volAttach.VolumeID, "error", err)
				return nil, fmt.Errorf("volume %s: %w", volAttach.VolumeID, err)
			}

			// Mark volume as attached (AttachVolume handles multi-attach validation)
			if err := m.volumeManager.AttachVolume(ctx, volAttach.VolumeID, volumes.AttachVolumeRequest{
				InstanceID: id,
				MountPath:  volAttach.MountPath,
				Readonly:   volAttach.Readonly,
			}); err != nil {
				log.ErrorContext(ctx, "failed to attach volume", "id", id, "volume_id", volAttach.VolumeID, "error", err)
				return nil, fmt.Errorf("attach volume %s: %w", volAttach.VolumeID, err)
			}

			// Add volume cleanup to stack
			volumeID := volAttach.VolumeID // capture for closure
			cu.Add(func() {
				m.volumeManager.DetachVolume(ctx, volumeID, id)
			})

			// Create overlay disk for volumes with overlay enabled
			if volAttach.Overlay {
				log.DebugContext(ctx, "creating volume overlay disk", "id", id, "volume_id", volAttach.VolumeID, "size", volAttach.OverlaySize)
				if err := m.createVolumeOverlayDisk(id, volAttach.VolumeID, volAttach.OverlaySize); err != nil {
					log.ErrorContext(ctx, "failed to create volume overlay disk", "id", id, "volume_id", volAttach.VolumeID, "error", err)
					return nil, fmt.Errorf("create volume overlay disk %s: %w", volAttach.VolumeID, err)
				}
			}
		}
		// Store volume attachments in metadata
		stored.Volumes = req.Volumes
	}

	// 11. Create config disk (needs Instance for buildVMConfig)
	inst := &Instance{StoredMetadata: *stored}
	log.DebugContext(ctx, "creating config disk", "id", id)
	if err := m.createConfigDisk(inst, imageInfo, netConfig); err != nil {
		log.ErrorContext(ctx, "failed to create config disk", "id", id, "error", err)
		return nil, fmt.Errorf("create config disk: %w", err)
	}

	// 12. Save metadata
	log.DebugContext(ctx, "saving instance metadata", "id", id)
	meta := &metadata{StoredMetadata: *stored}
	if err := m.saveMetadata(meta); err != nil {
		log.ErrorContext(ctx, "failed to save metadata", "id", id, "error", err)
		return nil, fmt.Errorf("save metadata: %w", err)
	}

	// 13. Start VMM and boot VM
	log.InfoContext(ctx, "starting VMM and booting VM", "id", id)
	if err := m.startAndBootVM(ctx, stored, imageInfo, netConfig); err != nil {
		log.ErrorContext(ctx, "failed to start and boot VM", "id", id, "error", err)
		return nil, err
	}

	// 14. Update timestamp after VM is running
	now := time.Now()
	stored.StartedAt = &now

	meta = &metadata{StoredMetadata: *stored}
	if err := m.saveMetadata(meta); err != nil {
		// VM is running but metadata failed - log but don't fail
		// Instance is recoverable, state will be derived
		log.WarnContext(ctx, "failed to update metadata after VM start", "id", id, "error", err)
	}

	// Success - release cleanup stack (prevent cleanup)
	cu.Release()

	// Return instance with derived state
	finalInst := m.toInstance(ctx, meta)
	log.InfoContext(ctx, "instance created successfully", "id", id, "name", req.Name, "state", finalInst.State)
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
	
	// Start VMM process and capture PID
	log.DebugContext(ctx, "starting VMM process", "id", stored.Id, "version", stored.CHVersion)
	pid, err := vmm.StartProcess(ctx, m.paths, stored.CHVersion, stored.SocketPath)
	if err != nil {
		return fmt.Errorf("start vmm: %w", err)
	}
	
	// Store the PID for later cleanup
	stored.CHPID = &pid
	log.DebugContext(ctx, "VMM process started", "id", stored.Id, "pid", pid)

	// Create VMM client
	client, err := vmm.NewVMM(stored.SocketPath)
	if err != nil {
		return fmt.Errorf("create vmm client: %w", err)
	}

	// Build VM configuration matching Cloud Hypervisor VmConfig
	inst := &Instance{StoredMetadata: *stored}
	vmConfig, err := m.buildVMConfig(inst, imageInfo, netConfig)
	if err != nil {
		return fmt.Errorf("build vm config: %w", err)
	}

	// Create VM in VMM
	log.DebugContext(ctx, "creating VM in VMM", "id", stored.Id)
	createResp, err := client.CreateVMWithResponse(ctx, vmConfig)
	if err != nil {
		return fmt.Errorf("create vm: %w", err)
	}
	if createResp.StatusCode() != 204 {
		// Include response body for debugging
		body := string(createResp.Body)
		log.ErrorContext(ctx, "create VM failed", "id", stored.Id, "status", createResp.StatusCode(), "body", body)
		return fmt.Errorf("create vm failed with status %d: %s", createResp.StatusCode(), body)
	}

	// Transition: Created → Running (boot VM)
	log.DebugContext(ctx, "booting VM", "id", stored.Id)
	bootResp, err := client.BootVMWithResponse(ctx)
	if err != nil {
		// Try to cleanup
		client.DeleteVMWithResponse(ctx)
		client.ShutdownVMMWithResponse(ctx)
		return fmt.Errorf("boot vm: %w", err)
	}
	if bootResp.StatusCode() != 204 {
		client.DeleteVMWithResponse(ctx)
		client.ShutdownVMMWithResponse(ctx)
		body := string(bootResp.Body)
		log.ErrorContext(ctx, "boot VM failed", "id", stored.Id, "status", bootResp.StatusCode(), "body", body)
		return fmt.Errorf("boot vm failed with status %d: %s", bootResp.StatusCode(), body)
	}

	// Optional: Expand memory to max if hotplug configured
	if inst.HotplugSize > 0 {
		totalBytes := inst.Size + inst.HotplugSize
		log.DebugContext(ctx, "expanding VM memory", "id", stored.Id, "total_bytes", totalBytes)
		resizeConfig := vmm.VmResize{DesiredRam: &totalBytes}
		// Best effort, ignore errors
		if resp, err := client.PutVmResizeWithResponse(ctx, resizeConfig); err != nil || resp.StatusCode() != 204 {
			log.WarnContext(ctx, "failed to expand VM memory", "id", stored.Id, "error", err)
		}
	}

	return nil
}

// buildVMConfig creates the Cloud Hypervisor VmConfig
func (m *manager) buildVMConfig(inst *Instance, imageInfo *images.Image, netConfig *network.NetworkConfig) (vmm.VmConfig, error) {
	// Get system file paths
	kernelPath, _ := m.systemManager.GetKernelPath(system.KernelVersion(inst.KernelVersion))
	initrdPath, _ := m.systemManager.GetInitrdPath()

	// Payload configuration (kernel + initramfs)
	payload := vmm.PayloadConfig{
		Kernel:    ptr(kernelPath),
		Cmdline:   ptr("console=ttyS0"),
		Initramfs: ptr(initrdPath),
	}

	// CPU configuration
	cpus := vmm.CpusConfig{
		BootVcpus: inst.Vcpus,
		MaxVcpus:  inst.Vcpus,
	}
	
	// Calculate and set guest topology based on host topology
	if topology := calculateGuestTopology(inst.Vcpus, m.hostTopology); topology != nil {
		cpus.Topology = topology
	}

	// Memory configuration
	memory := vmm.MemoryConfig{
		Size: inst.Size,
	}
	if inst.HotplugSize > 0 {
		memory.HotplugSize = &inst.HotplugSize
		memory.HotplugMethod = ptr("VirtioMem")  // PascalCase, not kebab-case
	}

	// Disk configuration
	// Get rootfs disk path from image manager
	rootfsPath, err := images.GetDiskPath(m.paths, imageInfo.Name, imageInfo.Digest)
	if err != nil {
		return vmm.VmConfig{}, err
	}

	disks := []vmm.DiskConfig{
		// Rootfs (from image, read-only)
		{
			Path:     &rootfsPath,
			Readonly: ptr(true),
		},
		// Overlay disk (writable)
		{
			Path: ptr(m.paths.InstanceOverlay(inst.Id)),
		},
		// Config disk (read-only)
		{
			Path:     ptr(m.paths.InstanceConfigDisk(inst.Id)),
			Readonly: ptr(true),
		},
	}

	// Add attached volumes as additional disks
	// For overlay volumes, add both base (readonly) and overlay disk
	for _, volAttach := range inst.Volumes {
		volumePath := m.volumeManager.GetVolumePath(volAttach.VolumeID)
		if volAttach.Overlay {
			// Base volume is always read-only when overlay is enabled
			disks = append(disks, vmm.DiskConfig{
				Path:     &volumePath,
				Readonly: ptr(true),
			})
			// Overlay disk is writable
			overlayPath := m.paths.InstanceVolumeOverlay(inst.Id, volAttach.VolumeID)
			disks = append(disks, vmm.DiskConfig{
				Path: &overlayPath,
			})
		} else {
			disks = append(disks, vmm.DiskConfig{
				Path:     &volumePath,
				Readonly: ptr(volAttach.Readonly),
			})
		}
	}

	// Serial console configuration
	serial := vmm.ConsoleConfig{
		Mode: vmm.ConsoleConfigMode("File"),
		File: ptr(m.paths.InstanceConsoleLog(inst.Id)),
	}

	// Console off (we use serial)
	console := vmm.ConsoleConfig{
		Mode: vmm.ConsoleConfigMode("Off"),
	}

	// Network configuration (optional, use passed config)
	var nets *[]vmm.NetConfig
	if netConfig != nil {
		nets = &[]vmm.NetConfig{{
			Tap:  &netConfig.TAPDevice,
			Ip:   &netConfig.IP,
			Mac:  &netConfig.MAC,
			Mask: &netConfig.Netmask,
		}}
	}

	// vsock configuration for remote exec
	vsock := vmm.VsockConfig{
		Cid:    inst.VsockCID,
		Socket: inst.VsockSocket,
	}

	return vmm.VmConfig{
		Payload: payload,
		Cpus:    &cpus,
		Memory:  &memory,
		Disks:   &disks,
		Serial:  &serial,
		Console: &console,
		Net:     nets,
		Vsock:   &vsock,
	}, nil
}

func ptr[T any](v T) *T {
	return &v
}

