//go:build linux

package devices

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/kernel/hypeman/lib/logger"
)

const (
	pciDevicesPath  = "/sys/bus/pci/devices"
	vfioDevicesPath = "/dev/vfio/devices"
)

type vendorVFIOSysfs struct {
	pciDevicesPath    string
	procPath          string
	vfioDevicesPath   string
	owners            map[string]string
	framebufferByType map[string]int
}

var (
	hostVendorVFIO = vendorVFIOSysfs{
		pciDevicesPath:    pciDevicesPath,
		procPath:          procPath,
		vfioDevicesPath:   vfioDevicesPath,
		owners:            make(map[string]string),
		framebufferByType: make(map[string]int),
	}
	vendorVFIOMu sync.Mutex
)

func (s vendorVFIOSysfs) discoverVFs() ([]VirtualFunction, error) {
	entries, err := os.ReadDir(s.pciDevicesPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read PCI devices: %w", err)
	}

	vfs := make([]VirtualFunction, 0)
	for _, entry := range entries {
		vfPath := filepath.Join(s.pciDevicesPath, entry.Name())
		nvidiaPath := filepath.Join(vfPath, "nvidia")
		if _, err := os.Stat(filepath.Join(nvidiaPath, "creatable_vgpu_types")); err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, fmt.Errorf("stat creatable vGPU types for VF %s: %w", entry.Name(), err)
		}

		currentType, err := readCurrentVGPUType(filepath.Join(nvidiaPath, "current_vgpu_type"))
		if err != nil {
			return nil, fmt.Errorf("read current vGPU type for VF %s: %w", entry.Name(), err)
		}

		parentGPU := ""
		if target, err := os.Readlink(filepath.Join(vfPath, "physfn")); err == nil {
			parentGPU = filepath.Base(target)
		}
		vfs = append(vfs, VirtualFunction{
			PCIAddress:  entry.Name(),
			ParentGPU:   parentGPU,
			Allocated:   currentType != "0",
			ProfileType: currentType,
		})
	}

	sort.Slice(vfs, func(i, j int) bool { return vfs[i].PCIAddress < vfs[j].PCIAddress })
	return vfs, nil
}

// listProfiles counts each free VF advertising a type as one creatable
// instance, matching the driver-reported units that mdev sums through
// available_instances. This is a best-effort snapshot because creating on one
// VF may revoke the type from siblings that share its GPU framebuffer.
func (s vendorVFIOSysfs) listProfiles(vfs []VirtualFunction) ([]GPUProfile, error) {
	profilesByType := make(map[string]profileMetadata)
	creatableVFs := make(map[string]int)
	for _, vf := range vfs {
		creatable, err := s.readCreatableProfiles(vf.PCIAddress)
		if err != nil {
			return nil, err
		}
		for _, profile := range creatable {
			profilesByType[profile.TypeName] = profile
			if !vf.Allocated {
				creatableVFs[profile.TypeName]++
			}
		}
	}

	metadata := make([]profileMetadata, 0, len(profilesByType))
	for _, profile := range profilesByType {
		metadata = append(metadata, profile)
	}
	sort.Slice(metadata, func(i, j int) bool { return metadata[i].Name < metadata[j].Name })

	profiles := make([]GPUProfile, 0, len(metadata))
	for _, profile := range metadata {
		profiles = append(profiles, GPUProfile{
			Name:          profile.Name,
			FramebufferMB: profile.FramebufferMB,
			Available:     creatableVFs[profile.TypeName],
		})
	}
	return profiles, nil
}

func (s vendorVFIOSysfs) create(ctx context.Context, profileName, instanceID string) (*VGPUDevice, error) {
	vendorVFIOMu.Lock()
	defer vendorVFIOMu.Unlock()

	vfs, err := s.discoverVFs()
	if err != nil {
		return nil, err
	}
	metadata, err := s.profileMetadata(vfs)
	if err != nil {
		return nil, err
	}

	var requested profileMetadata
	found := false
	for _, profile := range metadata {
		if profile.Name == profileName {
			requested = profile
			found = true
			break
		}
	}
	if !found {
		if len(metadata) == 0 && len(vfs) > 0 {
			return nil, fmt.Errorf("no creatable vGPU profiles on any VF, GPUs may be at capacity: profile %q", profileName)
		}
		return nil, fmt.Errorf("profile %q is not creatable on any VF (unknown profile or insufficient capacity)", profileName)
	}

	targetVF, err := s.selectLeastLoadedVF(vfs, requested.TypeName)
	if err != nil {
		return nil, err
	}
	if targetVF == "" {
		return nil, fmt.Errorf("no available VF for profile %q", profileName)
	}

	currentTypePath := filepath.Join(s.pciDevicesPath, targetVF, "nvidia", "current_vgpu_type")
	if err := os.WriteFile(currentTypePath, []byte(requested.TypeName), 0200); err != nil {
		return nil, fmt.Errorf("create vGPU on VF %s: %w", targetVF, err)
	}
	device := VGPUDevice{
		Framework:   VGPUFrameworkVendorVFIO,
		VFAddress:   targetVF,
		ProfileType: requested.TypeName,
		ProfileName: profileName,
		SysfsPath:   filepath.Join(s.pciDevicesPath, targetVF),
	}
	currentType, err := readCurrentVGPUType(currentTypePath)
	if err != nil {
		verifyErr := fmt.Errorf("verify vGPU on VF %s: %w", targetVF, err)
		return nil, s.rollbackCreate(currentTypePath, targetVF, instanceID, device, verifyErr)
	}
	if currentType != requested.TypeName {
		verifyErr := fmt.Errorf("verify vGPU on VF %s: type is %s, want %s", targetVF, currentType, requested.TypeName)
		return nil, s.rollbackCreate(currentTypePath, targetVF, instanceID, device, verifyErr)
	}
	s.owners[targetVF] = instanceID

	logger.FromContext(ctx).InfoContext(ctx, "created vendor VFIO vGPU",
		"profile", profileName,
		"vf", targetVF,
		"instance_id", instanceID,
	)
	return &device, nil
}

func (s vendorVFIOSysfs) destroy(ctx context.Context, vfAddress, instanceID string) error {
	return s.destroyWithOpenPaths(ctx, vfAddress, instanceID, nil)
}

func (s vendorVFIOSysfs) destroyWithOpenPaths(ctx context.Context, vfAddress, instanceID string, openPaths map[string]struct{}) error {
	vendorVFIOMu.Lock()
	defer vendorVFIOMu.Unlock()

	log := logger.FromContext(ctx)
	currentTypePath := filepath.Join(s.pciDevicesPath, vfAddress, "nvidia", "current_vgpu_type")
	currentType, err := readCurrentVGPUType(currentTypePath)
	if err != nil {
		if os.IsNotExist(err) {
			delete(s.owners, vfAddress)
			return nil
		}
		return fmt.Errorf("read current vGPU type for VF %s: %w", vfAddress, err)
	}
	if currentType == "0" {
		delete(s.owners, vfAddress)
		return nil
	}

	if owner, ok := s.owners[vfAddress]; ok {
		if instanceID == "" {
			return fmt.Errorf("cannot release vendor VFIO vGPU on VF %s without instance ID", vfAddress)
		}
		if owner != instanceID {
			log.WarnContext(ctx, "skipping vendor VFIO vGPU release owned by another instance",
				"vf", vfAddress,
				"owner_instance_id", owner,
				"requesting_instance_id", instanceID,
			)
			return nil
		}
	}

	if openPaths == nil {
		if openPaths, err = s.openVFIOPaths(); err != nil {
			return fmt.Errorf("scan open VFIO handles: %w", err)
		}
	}
	inUse, err := s.vfioDeviceInUse(vfAddress, openPaths)
	if err != nil {
		return fmt.Errorf("check vendor VFIO vGPU usage for VF %s: %w", vfAddress, err)
	}
	if inUse {
		return fmt.Errorf("vendor VFIO vGPU on VF %s is still in use", vfAddress)
	}

	if err := os.WriteFile(currentTypePath, []byte("0"), 0200); err != nil {
		return fmt.Errorf("destroy vGPU on VF %s: %w", vfAddress, err)
	}
	delete(s.owners, vfAddress)
	log.InfoContext(ctx, "destroyed vendor VFIO vGPU", "vf", vfAddress)
	return nil
}

func (s vendorVFIOSysfs) reconcile(ctx context.Context, protectedDevicePaths map[string]struct{}) error {
	vfs, err := s.discoverVFs()
	if err != nil {
		return err
	}
	log := logger.FromContext(ctx)
	protectedVFs := make(map[string]struct{}, len(protectedDevicePaths))
	for path := range protectedDevicePaths {
		protectedVFs[filepath.Base(path)] = struct{}{}
	}
	var openPaths map[string]struct{}
	for _, vf := range vfs {
		if !vf.Allocated {
			continue
		}
		if _, ok := protectedVFs[vf.PCIAddress]; ok {
			log.DebugContext(ctx, "skipping vendor VFIO vGPU held by a live instance", "vf", vf.PCIAddress)
			continue
		}
		if openPaths == nil {
			if openPaths, err = s.openVFIOPaths(); err != nil {
				return fmt.Errorf("scan open VFIO handles: %w", err)
			}
		}
		inUse, err := s.vfioDeviceInUse(vf.PCIAddress, openPaths)
		if err != nil {
			log.WarnContext(ctx, "failed to check vendor VFIO vGPU usage", "vf", vf.PCIAddress, "error", err)
			continue
		}
		if inUse {
			continue
		}
		if err := s.destroyWithOpenPaths(ctx, vf.PCIAddress, "", openPaths); err != nil {
			log.WarnContext(ctx, "failed to destroy orphaned vendor VFIO vGPU", "vf", vf.PCIAddress, "error", err)
		}
	}
	return nil
}

func (s vendorVFIOSysfs) selectLeastLoadedVF(vfs []VirtualFunction, profileType string) (string, error) {
	usageByGPU := make(map[string]int)
	unknownUsageByGPU := make(map[string]bool)
	freeByGPU := make(map[string][]VirtualFunction)
	for _, vf := range vfs {
		if vf.Allocated {
			// framebufferByType only covers currently creatable profiles, so
			// after a restart an allocated type can be missing when its
			// capacity is exhausted. Prefer GPUs whose load is fully known
			// instead of rejecting placement outright; the kernel driver
			// still enforces real capacity through creatable_vgpu_types.
			framebuffer, ok := s.framebufferByType[vf.ProfileType]
			if !ok {
				unknownUsageByGPU[vf.ParentGPU] = true
				continue
			}
			usageByGPU[vf.ParentGPU] += framebuffer
			continue
		}
		profiles, err := s.readCreatableProfiles(vf.PCIAddress)
		if err != nil {
			return "", err
		}
		for _, profile := range profiles {
			if profile.TypeName == profileType {
				freeByGPU[vf.ParentGPU] = append(freeByGPU[vf.ParentGPU], vf)
				break
			}
		}
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
		return "", nil
	}
	return freeByGPU[gpus[0]][0].PCIAddress, nil
}

func (s vendorVFIOSysfs) profileMetadata(vfs []VirtualFunction) ([]profileMetadata, error) {
	profilesByType := make(map[string]profileMetadata)
	for _, vf := range vfs {
		profiles, err := s.readCreatableProfiles(vf.PCIAddress)
		if err != nil {
			return nil, err
		}
		for _, profile := range profiles {
			profilesByType[profile.TypeName] = profile
			s.framebufferByType[profile.TypeName] = profile.FramebufferMB
		}
	}
	profiles := make([]profileMetadata, 0, len(profilesByType))
	for _, profile := range profilesByType {
		profiles = append(profiles, profile)
	}
	sort.Slice(profiles, func(i, j int) bool { return profiles[i].Name < profiles[j].Name })
	return profiles, nil
}

func (s vendorVFIOSysfs) readCreatableProfiles(vfAddress string) ([]profileMetadata, error) {
	path := filepath.Join(s.pciDevicesPath, vfAddress, "nvidia", "creatable_vgpu_types")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read creatable vGPU types for VF %s: %w", vfAddress, err)
	}
	return parseCreatableVGPUTypes(string(data))
}

func (s vendorVFIOSysfs) vfioDeviceInUse(vfAddress string, openPaths map[string]struct{}) (bool, error) {
	devicePaths := make([]string, 0, 2)
	probeErrs := make([]error, 0, 2)

	vfioDevices, err := os.ReadDir(filepath.Join(s.pciDevicesPath, vfAddress, "vfio-dev"))
	if err != nil {
		if !os.IsNotExist(err) {
			probeErrs = append(probeErrs, fmt.Errorf("read VFIO devices for VF %s: %w", vfAddress, err))
		}
	} else {
		for _, device := range vfioDevices {
			devicePaths = append(devicePaths, filepath.Join(s.vfioDevicesPath, device.Name()))
		}
	}

	target, err := os.Readlink(filepath.Join(s.pciDevicesPath, vfAddress, "iommu_group"))
	if err != nil {
		if !os.IsNotExist(err) {
			probeErrs = append(probeErrs, fmt.Errorf("read IOMMU group for VF %s: %w", vfAddress, err))
		}
	} else {
		devicePaths = append(devicePaths, filepath.Join(filepath.Dir(s.vfioDevicesPath), filepath.Base(target)))
	}

	for _, path := range devicePaths {
		if _, ok := openPaths[path]; ok {
			return true, nil
		}
	}
	if len(probeErrs) > 0 {
		return false, errors.Join(probeErrs...)
	}
	return false, nil
}

func (s vendorVFIOSysfs) openVFIOPaths() (map[string]struct{}, error) {
	processes, err := os.ReadDir(s.procPath)
	if err != nil {
		return nil, err
	}
	prefix := filepath.Dir(s.vfioDevicesPath) + string(filepath.Separator)
	open := make(map[string]struct{})
	for _, process := range processes {
		if _, err := strconv.Atoi(process.Name()); err != nil {
			continue
		}
		fdPath := filepath.Join(s.procPath, process.Name(), "fd")
		fds, err := os.ReadDir(fdPath)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, fmt.Errorf("read process %s file descriptors: %w", process.Name(), err)
		}
		for _, fd := range fds {
			target, err := os.Readlink(filepath.Join(fdPath, fd.Name()))
			if err != nil {
				if os.IsNotExist(err) {
					continue
				}
				return nil, fmt.Errorf("read process %s file descriptor %s: %w", process.Name(), fd.Name(), err)
			}
			if strings.HasPrefix(target, prefix) {
				open[target] = struct{}{}
			}
		}
	}
	return open, nil
}

func parseCreatableVGPUTypes(value string) ([]profileMetadata, error) {
	profiles := make([]profileMetadata, 0)
	for lineNumber, line := range strings.Split(value, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		typeID, name, found := strings.Cut(line, ":")
		typeID = strings.TrimSpace(typeID)
		name = strings.TrimSpace(name)
		if typeID == "ID" {
			continue
		}
		if !found || name == "" {
			return nil, fmt.Errorf("parse creatable vGPU types line %d: %q", lineNumber+1, line)
		}
		if _, err := strconv.Atoi(typeID); err != nil {
			return nil, fmt.Errorf("parse vGPU type ID %q: %w", typeID, err)
		}
		profiles = append(profiles, profileMetadata{
			TypeName:      typeID,
			Name:          name,
			FramebufferMB: framebufferFromProfileName(name),
		})
	}
	return profiles, nil
}

func framebufferFromProfileName(name string) int {
	series := strings.LastIndexAny(name, "ABCQ")
	if series <= 0 {
		return 0
	}
	dash := strings.LastIndex(name[:series], "-")
	if dash < 0 {
		return 0
	}
	gb, err := strconv.Atoi(name[dash+1 : series])
	if err != nil {
		return 0
	}
	return gb * 1024
}

func (s vendorVFIOSysfs) rollbackCreate(currentTypePath, vfAddress, instanceID string, device VGPUDevice, verifyErr error) error {
	if err := os.WriteFile(currentTypePath, []byte("0"), 0200); err != nil {
		s.owners[vfAddress] = instanceID
		return &VGPUCreateCleanupPendingError{
			Device: device,
			Err:    errors.Join(verifyErr, fmt.Errorf("roll back vGPU on VF %s: %w", vfAddress, err)),
		}
	}
	return verifyErr
}

func readCurrentVGPUType(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	value := strings.TrimSpace(string(data))
	if _, err := strconv.Atoi(value); err != nil {
		return "", fmt.Errorf("invalid current vGPU type %q", value)
	}
	return value, nil
}
