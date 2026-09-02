//go:build linux

package devices

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"

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
	openVFIOPathsFunc func() (map[string]struct{}, error)
}

var hostVendorVFIO = vendorVFIOSysfs{
	pciDevicesPath:  pciDevicesPath,
	procPath:        procPath,
	vfioDevicesPath: vfioDevicesPath,
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
	var vfErrs []error
	for _, entry := range entries {
		vfPath := filepath.Join(s.pciDevicesPath, entry.Name())
		nvidiaPath := filepath.Join(vfPath, "nvidia")
		if _, err := os.Stat(filepath.Join(nvidiaPath, "creatable_vgpu_types")); err != nil {
			if os.IsNotExist(err) {
				continue
			}
			vfErrs = append(vfErrs, fmt.Errorf("stat creatable vGPU types for VF %s: %w", entry.Name(), err))
			continue
		}

		currentType, err := readCurrentVGPUType(filepath.Join(nvidiaPath, "current_vgpu_type"))
		if err != nil {
			vfErrs = append(vfErrs, fmt.Errorf("read current vGPU type for VF %s: %w", entry.Name(), err))
			continue
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
	if len(vfErrs) > 0 {
		// One unreadable VF must not blank out the host's GPU capacity: skip
		// it and keep the readable inventory. A skipped VF is never selected
		// for placement and never reconciled, both safe directions. Only when
		// no VF is readable does discovery fail, so a wholesale sysfs outage
		// cannot demote a vGPU host to passthrough while assignments exist.
		if len(vfs) == 0 {
			return nil, errors.Join(vfErrs...)
		}
		slog.Default().Warn("skipping unreadable vendor VFIO VFs", "error", errors.Join(vfErrs...))
	}

	sort.Slice(vfs, func(i, j int) bool { return vfs[i].PCIAddress < vfs[j].PCIAddress })
	return vfs, nil
}

// listProfiles counts each free, non-quarantined VF advertising a type as
// one creatable instance, matching the driver-reported units that mdev sums
// through available_instances. This is a best-effort snapshot because
// creating on one VF may revoke the type from siblings that share its GPU
// framebuffer.
func (s vendorVFIOSysfs) listProfiles(vfs []VirtualFunction, quarantined map[string]struct{}) ([]GPUProfile, error) {
	profilesByType := make(map[string]VGPUProfileType)
	creatableVFs := make(map[string]int)
	profilesByVF, err := s.profileTypes(vfs)
	if err != nil {
		return nil, err
	}
	for _, vf := range vfs {
		_, bad := quarantined[vf.PCIAddress]
		for _, profile := range profilesByVF[vf.PCIAddress] {
			profilesByType[profile.TypeName] = profile
			if !vf.Allocated && !bad {
				creatableVFs[profile.TypeName]++
			}
		}
	}

	metadata := make([]VGPUProfileType, 0, len(profilesByType))
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

func (s vendorVFIOSysfs) profileTypes(vfs []VirtualFunction) (map[string][]VGPUProfileType, error) {
	profilesByVF := make(map[string][]VGPUProfileType, len(vfs))
	for _, vf := range vfs {
		profiles, err := s.readCreatableProfiles(vf.PCIAddress)
		if err != nil {
			slog.Default().Warn("skipping unreadable creatable vGPU types", "vf", vf.PCIAddress, "error", err)
			continue
		}
		converted := make([]VGPUProfileType, 0, len(profiles))
		for _, profile := range profiles {
			converted = append(converted, VGPUProfileType{
				TypeName:      profile.TypeName,
				Name:          profile.Name,
				FramebufferMB: profile.FramebufferMB,
			})
		}
		profilesByVF[vf.PCIAddress] = converted
	}
	return profilesByVF, nil
}

func (s vendorVFIOSysfs) configure(ctx context.Context, vfAddress, profileType string) error {
	vendorVFIOMu.Lock()
	defer vendorVFIOMu.Unlock()

	if profileType == "" || profileType == "0" {
		return fmt.Errorf("invalid vendor VFIO vGPU profile type %q", profileType)
	}
	// Placement filters quarantined VFs from a snapshot taken outside this
	// lock. Re-checking here, under the lock quarantine mutations take,
	// closes the window where a VF is quarantined between selection and
	// configuration.
	quarantined, err := vfHealth.checkedAddresses()
	if err != nil {
		return err
	}
	if _, bad := quarantined[vfAddress]; bad {
		return fmt.Errorf("vendor VFIO vGPU on VF %s is quarantined", vfAddress)
	}
	currentTypePath := filepath.Join(s.pciDevicesPath, vfAddress, "nvidia", "current_vgpu_type")
	currentType, err := readCurrentVGPUType(currentTypePath)
	if err != nil {
		return fmt.Errorf("read current vGPU type for VF %s: %w", vfAddress, err)
	}
	if currentType == profileType {
		return nil
	}
	if currentType != "0" {
		openPaths, err := s.openVFIOPaths()
		if err != nil {
			return fmt.Errorf("scan open VFIO handles: %w", err)
		}
		inUse, err := s.vfioDeviceInUse(vfAddress, openPaths)
		if err != nil {
			return fmt.Errorf("check vendor VFIO vGPU usage for VF %s: %w", vfAddress, err)
		}
		if inUse {
			return fmt.Errorf("vendor VFIO vGPU on VF %s is still in use", vfAddress)
		}
		if err := os.WriteFile(currentTypePath, []byte("0"), 0200); err != nil {
			return fmt.Errorf("reset vGPU on VF %s: %w", vfAddress, err)
		}
		currentType, err = readCurrentVGPUType(currentTypePath)
		if err != nil {
			return fmt.Errorf("verify reset vGPU on VF %s: %w", vfAddress, err)
		}
		if currentType != "0" {
			return fmt.Errorf("verify reset vGPU on VF %s: type is %s, want 0", vfAddress, currentType)
		}
	}
	if err := os.WriteFile(currentTypePath, []byte(profileType), 0200); err != nil {
		return fmt.Errorf("configure vGPU on VF %s: %w", vfAddress, err)
	}
	currentType, err = readCurrentVGPUType(currentTypePath)
	if err != nil {
		return fmt.Errorf("verify vGPU on VF %s: %w", vfAddress, err)
	}
	if currentType != profileType {
		return fmt.Errorf("verify vGPU on VF %s: type is %s, want %s", vfAddress, currentType, profileType)
	}
	logger.FromContext(ctx).InfoContext(ctx, "configured vendor VFIO vGPU", "profile_type", profileType, "vf", vfAddress)
	return nil
}

func (s vendorVFIOSysfs) destroy(ctx context.Context, vfAddress, instanceID string) error {
	vendorVFIOMu.Lock()
	defer vendorVFIOMu.Unlock()

	log := logger.FromContext(ctx)
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

	openPaths, err := s.openVFIOPaths()
	if err != nil {
		return fmt.Errorf("scan open VFIO handles: %w", err)
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
	log.InfoContext(ctx, "destroyed vendor VFIO vGPU", "vf", vfAddress, "instance_id", instanceID)
	return nil
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

// openVFIOPaths hard-fails on any unreadable /proc entry, unlike mdev's
// isVFIOGroupInUse which skips them. That is deliberate: this scan authorizes
// clearing current_vgpu_type on a VF path that is reused across assignments,
// so an incomplete scan must fail the release (which retains metadata for a
// later retry) rather than risk a false "not in use" answer. The exception
// is a process that exits mid-scan (ENOENT/ESRCH): a dead process holds
// nothing open, so skipping it cannot produce that false answer.
func (s vendorVFIOSysfs) openVFIOPaths() (map[string]struct{}, error) {
	if s.openVFIOPathsFunc != nil {
		return s.openVFIOPathsFunc()
	}
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
			if os.IsNotExist(err) || errors.Is(err, syscall.ESRCH) {
				continue
			}
			return nil, fmt.Errorf("read process %s file descriptors: %w", process.Name(), err)
		}
		for _, fd := range fds {
			target, err := os.Readlink(filepath.Join(fdPath, fd.Name()))
			if err != nil {
				if os.IsNotExist(err) || errors.Is(err, syscall.ESRCH) {
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

// framebufferFromProfileName parses the framebuffer size from names like
// "NVIDIA L40S-12Q". NVIDIA's 512 MB 0Q/0B profiles parse as 0 and would read
// as free VRAM in least-loaded placement; no supported GPU exposes them today,
// so placement only skews if such a profile ever appears.
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
