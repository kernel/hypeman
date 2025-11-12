package instances

import (
	"context"
	"fmt"
	"path/filepath"
	"time"

	"github.com/oklog/ulid/v2"
	"github.com/onkernel/hypeman/lib/images"
	"github.com/onkernel/hypeman/lib/system"
	"github.com/onkernel/hypeman/lib/vmm"
)

// createInstance creates and starts a new instance
// Multi-hop orchestration: Stopped → Created → Running
func (m *manager) createInstance(
	ctx context.Context,
	req CreateInstanceRequest,
) (*Instance, error) {
	// 1. Validate request
	if err := validateCreateRequest(req); err != nil {
		return nil, err
	}

	// 2. Validate image exists and is ready
	imageInfo, err := m.imageManager.GetImage(ctx, req.Image)
	if err != nil {
		if err == images.ErrNotFound {
			return nil, fmt.Errorf("image %s: %w", req.Image, err)
		}
		return nil, fmt.Errorf("get image: %w", err)
	}

	if imageInfo.Status != images.StatusReady {
		return nil, fmt.Errorf("%w: image status is %s", ErrImageNotReady, imageInfo.Status)
	}

	// 3. Generate instance ID (ULID for time-ordered IDs)
	id := ulid.Make().String()

	// 4. Check instance doesn't already exist
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
	if overlaySize > m.maxOverlaySize {
		return nil, fmt.Errorf("overlay size %d exceeds maximum allowed size %d", overlaySize, m.maxOverlaySize)
	}
	vcpus := req.Vcpus
	if vcpus == 0 {
		vcpus = 2
	}
	if req.Env == nil {
		req.Env = make(map[string]string)
	}

	// 6. Get default system versions
	kernelVer, initrdVer := m.systemManager.GetDefaultVersions()

	// 7. Create instance metadata
	stored := &StoredMetadata{
		Id:            id,
		Name:          req.Name,
		Image:         req.Image,
		Size:          size,
		HotplugSize:   hotplugSize,
		OverlaySize:   overlaySize,
		Vcpus:         vcpus,
		Env:           req.Env,
		CreatedAt:     time.Now(),
		StartedAt:     nil,
		StoppedAt:     nil,
		KernelVersion: string(kernelVer),
		InitrdVersion: string(initrdVer),
		CHVersion:     vmm.V49_0, // Use latest
		SocketPath:    filepath.Join(m.dataDir, "guests", id, "ch.sock"),
		DataDir:       filepath.Join(m.dataDir, "guests", id),
	}

	// 8. Ensure directories
	if err := m.ensureDirectories(id); err != nil {
		return nil, fmt.Errorf("ensure directories: %w", err)
	}

	// 9. Create overlay disk with specified size
	if err := m.createOverlayDisk(id, stored.OverlaySize); err != nil {
		m.deleteInstanceData(id) // Cleanup
		return nil, fmt.Errorf("create overlay disk: %w", err)
	}

	// 10. Create config disk (needs Instance for buildVMConfig)
	inst := &Instance{StoredMetadata: *stored}
	if err := m.createConfigDisk(inst, imageInfo); err != nil {
		m.deleteInstanceData(id) // Cleanup
		return nil, fmt.Errorf("create config disk: %w", err)
	}

	// 11. Save metadata
	meta := &metadata{StoredMetadata: *stored}
	if err := m.saveMetadata(meta); err != nil {
		m.deleteInstanceData(id) // Cleanup
		return nil, fmt.Errorf("save metadata: %w", err)
	}

	// 12. Start VMM and boot VM
	if err := m.startAndBootVM(ctx, stored, imageInfo); err != nil {
		m.deleteInstanceData(id) // Cleanup
		return nil, err
	}

	// 13. Update timestamp after VM is running
	now := time.Now()
	stored.StartedAt = &now

	meta = &metadata{StoredMetadata: *stored}
	if err := m.saveMetadata(meta); err != nil {
		// VM is running but metadata failed - log but don't fail
		// Instance is recoverable, state will be derived
	}

	// Return instance with derived state
	finalInst := m.toInstance(ctx, meta)
	return &finalInst, nil
}

// validateCreateRequest validates the create instance request
func validateCreateRequest(req CreateInstanceRequest) error {
	if req.Name == "" {
		return fmt.Errorf("name is required")
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
	return nil
}

// startAndBootVM starts the VMM and boots the VM
func (m *manager) startAndBootVM(
	ctx context.Context,
	stored *StoredMetadata,
	imageInfo *images.Image,
) error {
	// Start VMM process and capture PID
	pid, err := vmm.StartProcess(ctx, m.dataDir, stored.CHVersion, stored.SocketPath)
	if err != nil {
		return fmt.Errorf("start vmm: %w", err)
	}
	
	// Store the PID for later cleanup
	stored.CHPID = &pid

	// Create VMM client
	client, err := vmm.NewVMM(stored.SocketPath)
	if err != nil {
		return fmt.Errorf("create vmm client: %w", err)
	}

	// Build VM configuration matching Cloud Hypervisor VmConfig
	inst := &Instance{StoredMetadata: *stored}
	vmConfig, err := m.buildVMConfig(inst, imageInfo)
	if err != nil {
		return fmt.Errorf("build vm config: %w", err)
	}

	// Create VM in VMM
	createResp, err := client.CreateVMWithResponse(ctx, vmConfig)
	if err != nil {
		return fmt.Errorf("create vm: %w", err)
	}
	if createResp.StatusCode() != 204 {
		// Include response body for debugging
		body := string(createResp.Body)
		return fmt.Errorf("create vm failed with status %d: %s", createResp.StatusCode(), body)
	}

	// Transition: Created → Running (boot VM)
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
		return fmt.Errorf("boot vm failed with status %d: %s", bootResp.StatusCode(), body)
	}

	// Optional: Expand memory to max if hotplug configured
	if inst.HotplugSize > 0 {
		totalBytes := inst.Size + inst.HotplugSize
		resizeConfig := vmm.VmResize{DesiredRam: &totalBytes}
		// Best effort, ignore errors
		client.PutVmResizeWithResponse(ctx, resizeConfig)
	}

	return nil
}

// buildVMConfig creates the Cloud Hypervisor VmConfig
func (m *manager) buildVMConfig(inst *Instance, imageInfo *images.Image) (vmm.VmConfig, error) {
	// Get versioned system file paths
	kernelPath, _ := m.systemManager.GetKernelPath(system.KernelVersion(inst.KernelVersion))
	initrdPath, _ := m.systemManager.GetInitrdPath(system.InitrdVersion(inst.InitrdVersion))

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
	rootfsPath, err := images.GetDiskPath(m.dataDir, imageInfo.Name, imageInfo.Digest)
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
			Path: ptr(filepath.Join(inst.DataDir, "overlay.raw")),
		},
		// Config disk (read-only)
		{
			Path:     ptr(filepath.Join(inst.DataDir, "config.ext4")),
			Readonly: ptr(true),
		},
	}

	// Serial console configuration
	serial := vmm.ConsoleConfig{
		Mode: vmm.ConsoleConfigMode("File"),
		File: ptr(filepath.Join(inst.DataDir, "logs", "console.log")),
	}

	// Console off (we use serial)
	console := vmm.ConsoleConfig{
		Mode: vmm.ConsoleConfigMode("Off"),
	}

	return vmm.VmConfig{
		Payload: payload,
		Cpus:    &cpus,
		Memory:  &memory,
		Disks:   &disks,
		Serial:  &serial,
		Console: &console,
	}, nil
}

func ptr[T any](v T) *T {
	return &v
}

