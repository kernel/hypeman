package instances

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"

	"github.com/kernel/hypeman/lib/devices"
	"github.com/kernel/hypeman/lib/logger"
)

func (m *manager) createVGPUDevice(ctx context.Context, profileName, instanceID string) (*devices.VGPUDevice, error) {
	create := m.createVGPU
	if create == nil {
		create = devices.CreateVGPU
	}
	return create(ctx, profileName, instanceID)
}

func (m *manager) discoverVGPUDevices() (devices.VGPUFramework, []devices.VirtualFunction, error) {
	discover := m.discoverVGPU
	if discover == nil {
		discover = devices.DiscoverVGPU
	}
	return discover()
}

func (m *manager) claimVGPU(ctx context.Context, meta *metadata, profileName string) (*devices.VGPUDevice, error) {
	framework, _, err := m.discoverVGPUDevices()
	if err != nil {
		return nil, err
	}
	if framework != devices.VGPUFrameworkVendorVFIO {
		return m.createVGPUDevice(ctx, profileName, meta.Id)
	}

	m.vgpuAllocationMu.Lock()
	defer m.vgpuAllocationMu.Unlock()

	allMetadata, err := m.listMetadataForReconcile()
	if err != nil {
		return nil, fmt.Errorf("list instances for vGPU allocation: %w", err)
	}
	framework, vfs, err := m.discoverVGPUDevices()
	if err != nil {
		return nil, err
	}
	if framework != devices.VGPUFrameworkVendorVFIO {
		return nil, fmt.Errorf("vendor VFIO vGPU framework not available")
	}
	listProfiles := m.vendorVFIOProfiles
	if listProfiles == nil {
		listProfiles = devices.ListVendorVFIOProfileTypes
	}
	profilesByVF, err := listProfiles(vfs)
	if err != nil {
		return nil, err
	}
	vfAddress, profileType, err := selectVendorVFIOVF(vfs, profilesByVF, allMetadata, profileName)
	if err != nil {
		// A dirty unclaimed VF consumes framebuffer, which can make the
		// requested profile vanish from every creatable list before the
		// selector's repair path can resolve it. Reset such VFs and retry.
		if !m.resetDirtyUnclaimedVFs(ctx, vfs, allMetadata) {
			return nil, err
		}
		if _, vfs, err = m.discoverVGPUDevices(); err != nil {
			return nil, err
		}
		if profilesByVF, err = listProfiles(vfs); err != nil {
			return nil, err
		}
		if vfAddress, profileType, err = selectVendorVFIOVF(vfs, profilesByVF, allMetadata, profileName); err != nil {
			return nil, err
		}
	}
	device := &devices.VGPUDevice{
		Framework:   devices.VGPUFrameworkVendorVFIO,
		VFAddress:   vfAddress,
		ProfileType: profileType,
		ProfileName: profileName,
		SysfsPath:   filepath.Clean(devices.GetDeviceSysfsPath(vfAddress)),
	}
	setStoredVGPUDevice(&meta.StoredMetadata, device)
	if err := m.saveMetadata(meta); err != nil {
		clearStoredVGPUDevice(&meta.StoredMetadata)
		return nil, fmt.Errorf("save vGPU claim: %w", err)
	}
	return device, nil
}

// resetDirtyUnclaimedVFs resets allocated VFs that no instance claims,
// restoring the framebuffer their leftover type consumes. The reset is
// gated on the open-VFIO-handle in-use check inside destroy. Reports
// whether any VF was reset. Callers must hold vgpuAllocationMu.
func (m *manager) resetDirtyUnclaimedVFs(ctx context.Context, vfs []devices.VirtualFunction, allMetadata []StoredMetadata) bool {
	claimed := make(map[string]struct{}, len(allMetadata))
	for i := range allMetadata {
		if path := allMetadata[i].GPUDevicePath; path != "" {
			claimed[filepath.Base(path)] = struct{}{}
		}
	}
	log := logger.FromContext(ctx)
	reset := false
	for _, vf := range vfs {
		if !vf.Allocated {
			continue
		}
		if _, ok := claimed[vf.PCIAddress]; ok {
			continue
		}
		assignment := devices.VGPUAssignment{
			Framework:  devices.VGPUFrameworkVendorVFIO,
			DevicePath: filepath.Clean(devices.GetDeviceSysfsPath(vf.PCIAddress)),
		}
		if err := m.destroyVGPUAssignment(ctx, assignment); err != nil {
			log.WarnContext(ctx, "failed to reset dirty vGPU VF", "vf", vf.PCIAddress, "error", err)
			continue
		}
		log.InfoContext(ctx, "reset dirty vGPU VF during allocation", "vf", vf.PCIAddress)
		reset = true
	}
	return reset
}

func selectVendorVFIOVF(vfs []devices.VirtualFunction, profilesByVF map[string][]devices.VGPUProfileType, allMetadata []StoredMetadata, profileName string) (string, string, error) {
	profilesByName := make(map[string]devices.VGPUProfileType)
	advertises := make(map[string]map[string]struct{}, len(profilesByVF))
	for vfAddress, profiles := range profilesByVF {
		advertises[vfAddress] = make(map[string]struct{}, len(profiles))
		for _, profile := range profiles {
			profilesByName[profile.Name] = profile
			advertises[vfAddress][profile.TypeName] = struct{}{}
		}
	}
	requested, found := profilesByName[profileName]
	if !found {
		if len(profilesByName) == 0 && len(vfs) > 0 {
			return "", "", fmt.Errorf("no creatable vGPU profiles on any VF, GPUs may be at capacity: profile %q", profileName)
		}
		return "", "", fmt.Errorf("profile %q is not creatable on any VF (unknown profile or insufficient capacity)", profileName)
	}

	vfByAddress := make(map[string]devices.VirtualFunction, len(vfs))
	for _, vf := range vfs {
		vfByAddress[vf.PCIAddress] = vf
	}
	claimed := make(map[string]struct{})
	usageByGPU := make(map[string]int)
	unknownUsageByGPU := make(map[string]bool)
	for i := range allMetadata {
		stored := &allMetadata[i]
		if stored.GPUDevicePath == "" {
			continue
		}
		vfAddress := filepath.Base(stored.GPUDevicePath)
		claimed[vfAddress] = struct{}{}
		vf, ok := vfByAddress[vfAddress]
		if !ok {
			continue
		}
		profile, ok := profilesByName[stored.GPUProfile]
		if !ok {
			unknownUsageByGPU[vf.ParentGPU] = true
			continue
		}
		usageByGPU[vf.ParentGPU] += profile.FramebufferMB
	}

	parentAdvertises := make(map[string]bool)
	for _, vf := range vfs {
		if _, ok := advertises[vf.PCIAddress][requested.TypeName]; ok {
			parentAdvertises[vf.ParentGPU] = true
		}
	}
	freeByGPU := make(map[string][]devices.VirtualFunction)
	for _, vf := range vfs {
		if _, ok := claimed[vf.PCIAddress]; ok {
			continue
		}
		_, directlyCreatable := advertises[vf.PCIAddress][requested.TypeName]
		repairable := vf.Allocated && (vf.ProfileType == requested.TypeName || parentAdvertises[vf.ParentGPU])
		if directlyCreatable || repairable {
			freeByGPU[vf.ParentGPU] = append(freeByGPU[vf.ParentGPU], vf)
		}
	}
	for gpu := range freeByGPU {
		sort.Slice(freeByGPU[gpu], func(i, j int) bool {
			return freeByGPU[gpu][i].PCIAddress < freeByGPU[gpu][j].PCIAddress
		})
	}
	gpus := make([]string, 0, len(freeByGPU))
	for gpu := range freeByGPU {
		gpus = append(gpus, gpu)
	}
	sort.Slice(gpus, func(i, j int) bool {
		if unknownUsageByGPU[gpus[i]] != unknownUsageByGPU[gpus[j]] {
			return !unknownUsageByGPU[gpus[i]]
		}
		if usageByGPU[gpus[i]] == usageByGPU[gpus[j]] {
			return gpus[i] < gpus[j]
		}
		return usageByGPU[gpus[i]] < usageByGPU[gpus[j]]
	})
	if len(gpus) == 0 {
		return "", "", fmt.Errorf("no available VF for profile %q", profileName)
	}
	return freeByGPU[gpus[0]][0].PCIAddress, requested.TypeName, nil
}

func (m *manager) configureClaimedVGPU(ctx context.Context, device *devices.VGPUDevice) error {
	if device.Framework != devices.VGPUFrameworkVendorVFIO {
		return nil
	}
	configure := m.configureVGPU
	if configure == nil {
		configure = devices.ConfigureVGPU
	}
	return configure(ctx, device.VFAddress, device.ProfileType)
}

func (m *manager) destroyVGPUAssignment(ctx context.Context, assignment devices.VGPUAssignment) error {
	destroy := m.destroyVGPU
	if destroy == nil {
		destroy = devices.DestroyVGPU
	}
	return destroy(ctx, assignment)
}

func setStoredVGPUDevice(stored *StoredMetadata, device *devices.VGPUDevice) {
	stored.GPUFramework = device.Framework
	stored.GPUDevicePath = device.SysfsPath
	stored.GPUMdevUUID = device.MdevUUID
}

func clearStoredVGPUDevice(stored *StoredMetadata) {
	stored.GPUFramework = devices.VGPUFrameworkNone
	stored.GPUDevicePath = ""
	stored.GPUMdevUUID = ""
}

func (m *manager) cleanupCreateVGPU(ctx context.Context, stored *StoredMetadata) bool {
	path := storedVGPUDevicePath(stored)
	if path == "" {
		return true
	}
	if hypervisorMayBeAlive(stored.HypervisorProcessIdentity, stored.SocketPath) {
		logger.FromContext(ctx).WarnContext(ctx, "preserving vGPU claim because hypervisor liveness is not clear", "instance_id", stored.Id)
		return false
	}
	if stored.GPUFramework == devices.VGPUFrameworkVendorVFIO {
		onDisk, err := m.loadMetadata(stored.Id)
		if err != nil || onDisk.GPUDevicePath != stored.GPUDevicePath {
			logger.FromContext(ctx).WarnContext(ctx, "skipping vGPU cleanup without matching claim", "instance_id", stored.Id, "device_path", path, "error", err)
			return false
		}
	}
	if err := m.destroyVGPUAssignment(ctx, vgpuAssignment(stored)); err != nil {
		logger.FromContext(ctx).WarnContext(ctx, "failed to reset vGPU during create cleanup", "instance_id", stored.Id, "device_path", path, "error", err)
	}
	return true
}

func (m *manager) cleanupStartVGPU(ctx context.Context, current *StoredMetadata, rollback metadata) {
	path := storedVGPUDevicePath(current)
	if path == "" {
		return
	}
	if hypervisorMayBeAlive(current.HypervisorProcessIdentity, current.SocketPath) {
		logger.FromContext(ctx).WarnContext(ctx, "preserving vGPU claim because hypervisor liveness is not clear", "instance_id", current.Id, "device_path", path)
		return
	}
	if current.GPUFramework == devices.VGPUFrameworkVendorVFIO {
		onDisk, err := m.loadMetadata(current.Id)
		if err != nil || onDisk.GPUDevicePath != current.GPUDevicePath {
			logger.FromContext(ctx).WarnContext(ctx, "skipping vGPU cleanup without matching claim", "instance_id", current.Id, "device_path", path, "error", err)
			return
		}
	}
	if err := m.destroyVGPUAssignment(ctx, vgpuAssignment(current)); err != nil {
		logger.FromContext(ctx).WarnContext(ctx, "failed to reset vGPU during start cleanup", "instance_id", current.Id, "device_path", path, "error", err)
	}
	m.vgpuAllocationMu.Lock()
	defer m.vgpuAllocationMu.Unlock()
	onDisk, err := m.loadMetadata(current.Id)
	if err != nil || storedVGPUDevicePath(&onDisk.StoredMetadata) != path {
		return
	}
	if err := m.saveMetadata(&rollback); err != nil {
		logger.FromContext(ctx).ErrorContext(ctx, "failed to restore metadata after vGPU cleanup", "instance_id", current.Id, "error", err)
	}
}

func (m *manager) releaseStoredVGPU(ctx context.Context, stored *StoredMetadata) error {
	if storedVGPUDevicePath(stored) != "" {
		if err := m.destroyVGPUAssignment(ctx, vgpuAssignment(stored)); err != nil {
			return err
		}
	}
	clearStoredVGPUDevice(stored)
	return nil
}

func (m *manager) releaseStoredVGPUPersisted(ctx context.Context, meta *metadata) error {
	if err := m.releaseStoredVGPU(ctx, &meta.StoredMetadata); err != nil {
		return err
	}
	m.vgpuAllocationMu.Lock()
	defer m.vgpuAllocationMu.Unlock()
	return m.saveMetadata(meta)
}

func vgpuAssignment(stored *StoredMetadata) devices.VGPUAssignment {
	return devices.VGPUAssignment{
		Framework:  stored.GPUFramework,
		DevicePath: storedVGPUDevicePath(stored),
		MdevUUID:   stored.GPUMdevUUID,
		InstanceID: stored.Id,
	}
}

func storedVGPUDevicePath(stored *StoredMetadata) string {
	if stored.GPUDevicePath != "" {
		return stored.GPUDevicePath
	}
	if stored.GPUMdevUUID != "" {
		return filepath.Join("/sys/bus/mdev/devices", stored.GPUMdevUUID)
	}
	return ""
}
