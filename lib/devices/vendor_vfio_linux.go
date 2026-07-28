//go:build linux

package devices

import (
	"context"
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
	pciDevicesPath  string
	procPath        string
	vfioDevicesPath string
}

var (
	hostVendorVFIO = vendorVFIOSysfs{
		pciDevicesPath:  pciDevicesPath,
		procPath:        procPath,
		vfioDevicesPath: vfioDevicesPath,
	}
	vendorVFIOMu sync.Mutex
)

func (s vendorVFIOSysfs) available() bool {
	vfs, err := s.discoverVFs()
	return err == nil && len(vfs) > 0
}

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
			continue
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

func (s vendorVFIOSysfs) listProfiles(vfs []VirtualFunction) ([]GPUProfile, error) {
	metadata, err := s.profileMetadata(vfs)
	if err != nil {
		return nil, err
	}

	availability := make(map[string]int, len(metadata))
	for _, vf := range vfs {
		if vf.Allocated {
			continue
		}
		profiles, err := s.readCreatableProfiles(vf.PCIAddress)
		if err != nil {
			return nil, err
		}
		for _, profile := range profiles {
			availability[profile.TypeName]++
		}
	}

	profiles := make([]GPUProfile, 0, len(metadata))
	for _, profile := range metadata {
		profiles = append(profiles, GPUProfile{
			Name:          profile.Name,
			FramebufferMB: profile.FramebufferMB,
			Available:     availability[profile.TypeName],
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
		return nil, fmt.Errorf("profile %q not found", profileName)
	}

	targetVF, err := s.selectLeastLoadedVF(vfs, metadata, requested.TypeName)
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
	currentType, err := readCurrentVGPUType(currentTypePath)
	if err != nil {
		_ = os.WriteFile(currentTypePath, []byte("0"), 0200)
		return nil, fmt.Errorf("verify vGPU on VF %s: %w", targetVF, err)
	}
	if currentType != requested.TypeName {
		_ = os.WriteFile(currentTypePath, []byte("0"), 0200)
		return nil, fmt.Errorf("verify vGPU on VF %s: type is %s, want %s", targetVF, currentType, requested.TypeName)
	}

	logger.FromContext(ctx).InfoContext(ctx, "created vendor VFIO vGPU",
		"profile", profileName,
		"vf", targetVF,
		"instance_id", instanceID,
	)
	return &VGPUDevice{
		Framework:   VGPUFrameworkVendorVFIO,
		VFAddress:   targetVF,
		ProfileType: requested.TypeName,
		ProfileName: profileName,
		SysfsPath:   filepath.Join(s.pciDevicesPath, targetVF),
	}, nil
}

func (s vendorVFIOSysfs) destroy(ctx context.Context, vfAddress string) error {
	vendorVFIOMu.Lock()
	defer vendorVFIOMu.Unlock()

	currentTypePath := filepath.Join(s.pciDevicesPath, vfAddress, "nvidia", "current_vgpu_type")
	currentType, err := readCurrentVGPUType(currentTypePath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read current vGPU type for VF %s: %w", vfAddress, err)
	}
	if currentType == "0" {
		return nil
	}
	if err := os.WriteFile(currentTypePath, []byte("0"), 0200); err != nil {
		return fmt.Errorf("destroy vGPU on VF %s: %w", vfAddress, err)
	}
	logger.FromContext(ctx).InfoContext(ctx, "destroyed vendor VFIO vGPU", "vf", vfAddress)
	return nil
}

func (s vendorVFIOSysfs) reconcile(ctx context.Context) error {
	vfs, err := s.discoverVFs()
	if err != nil {
		return err
	}
	log := logger.FromContext(ctx)
	for _, vf := range vfs {
		if !vf.Allocated {
			continue
		}
		inUse, err := s.vfioDeviceInUse(vf.PCIAddress)
		if err != nil {
			log.WarnContext(ctx, "failed to check vendor VFIO vGPU usage", "vf", vf.PCIAddress, "error", err)
			continue
		}
		if inUse {
			continue
		}
		if err := s.destroy(ctx, vf.PCIAddress); err != nil {
			log.WarnContext(ctx, "failed to destroy orphaned vendor VFIO vGPU", "vf", vf.PCIAddress, "error", err)
		}
	}
	return nil
}

func (s vendorVFIOSysfs) selectLeastLoadedVF(vfs []VirtualFunction, metadata []profileMetadata, profileType string) (string, error) {
	framebufferByType := make(map[string]int, len(metadata))
	for _, profile := range metadata {
		framebufferByType[profile.TypeName] = profile.FramebufferMB
	}

	usageByGPU := make(map[string]int)
	freeByGPU := make(map[string][]VirtualFunction)
	for _, vf := range vfs {
		if vf.Allocated {
			usageByGPU[vf.ParentGPU] += framebufferByType[vf.ProfileType]
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

func (s vendorVFIOSysfs) vfioDeviceInUse(vfAddress string) (bool, error) {
	vfioDevices, err := os.ReadDir(filepath.Join(s.pciDevicesPath, vfAddress, "vfio-dev"))
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	devicePaths := make(map[string]struct{}, len(vfioDevices))
	for _, device := range vfioDevices {
		devicePaths[filepath.Join(s.vfioDevicesPath, device.Name())] = struct{}{}
	}

	processes, err := os.ReadDir(s.procPath)
	if err != nil {
		return false, err
	}
	for _, process := range processes {
		if _, err := strconv.Atoi(process.Name()); err != nil {
			continue
		}
		fdPath := filepath.Join(s.procPath, process.Name(), "fd")
		fds, err := os.ReadDir(fdPath)
		if err != nil {
			continue
		}
		for _, fd := range fds {
			target, err := os.Readlink(filepath.Join(fdPath, fd.Name()))
			if err != nil {
				continue
			}
			if _, ok := devicePaths[target]; ok {
				return true, nil
			}
		}
	}
	return false, nil
}

func parseCreatableVGPUTypes(value string) ([]profileMetadata, error) {
	profiles := make([]profileMetadata, 0)
	for lineNumber, line := range strings.Split(value, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		if len(fields) < 2 {
			return nil, fmt.Errorf("parse creatable vGPU types line %d: %q", lineNumber+1, line)
		}
		typeID := fields[len(fields)-1]
		if _, err := strconv.Atoi(typeID); err != nil {
			return nil, fmt.Errorf("parse vGPU type ID %q: %w", typeID, err)
		}
		name := strings.Join(fields[:len(fields)-1], " ")
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
