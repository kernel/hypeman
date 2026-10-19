//go:build linux

package devices

import (
	"fmt"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"unsafe"

	"github.com/ebitengine/purego"
)

// nvmlLibrary is the NVML library the NVIDIA driver installs. It is loaded
// at runtime, so hosts without the driver never need it.
var nvmlLibrary = "libnvidia-ml.so.1"

const (
	nvmlSuccess          = 0
	nvmlInsufficientSize = 7
	nvmlFeatureEnabled   = 1
)

// nvmlPlacementList is nvmlVgpuPlacementList_v2_t.
type nvmlPlacementList struct {
	version       uint32
	placementSize uint32
	count         uint32
	placementIDs  *uint32
	mode          uint32
}

// nvmlHeterogeneousMode is nvmlVgpuHeterogeneousMode_v1_t.
type nvmlHeterogeneousMode struct {
	version uint32
	mode    uint32
}

// nvmlStructVersion is NVML_STRUCT_VERSION.
func nvmlStructVersion(size uintptr, version uint32) uint32 {
	return uint32(size) | version<<24
}

type nvmlFuncs struct {
	errorString                  func(rc int32) string
	deviceGetHandleByPCIBusID    func(busID string, device *uintptr) int32
	deviceGetHeterogeneousMode   func(device uintptr, mode *nvmlHeterogeneousMode) int32
	deviceGetSupportedVGPUs      func(device uintptr, count *uint32, typeIDs *uint32) int32
	deviceGetCreatablePlacements func(device uintptr, typeID uint32, list *nvmlPlacementList) int32
	vgpuTypeGetName              func(typeID uint32, name *byte, size *uint32) int32
}

var (
	nvmlMu sync.Mutex
	nvml   *nvmlFuncs
)

// openNVML loads and initializes NVML on first use and keeps it for the life
// of the process. A failure is not kept, so a driver that loads later is
// picked up by the next query.
func openNVML() (*nvmlFuncs, error) {
	nvmlMu.Lock()
	defer nvmlMu.Unlock()
	if nvml != nil {
		return nvml, nil
	}
	lib, err := purego.Dlopen(nvmlLibrary, purego.RTLD_NOW|purego.RTLD_GLOBAL)
	if err != nil {
		return nil, fmt.Errorf("load %s: %w", nvmlLibrary, err)
	}
	var f nvmlFuncs
	var initialize func() int32
	// purego.RegisterLibFunc panics on a missing symbol, which an older
	// driver could cause, so each symbol is looked up first.
	for name, fn := range map[string]any{
		"nvmlInit_v2":                              &initialize,
		"nvmlErrorString":                          &f.errorString,
		"nvmlDeviceGetHandleByPciBusId_v2":         &f.deviceGetHandleByPCIBusID,
		"nvmlDeviceGetVgpuHeterogeneousMode":       &f.deviceGetHeterogeneousMode,
		"nvmlDeviceGetSupportedVgpus":              &f.deviceGetSupportedVGPUs,
		"nvmlDeviceGetVgpuTypeCreatablePlacements": &f.deviceGetCreatablePlacements,
		"nvmlVgpuTypeGetName":                      &f.vgpuTypeGetName,
	} {
		sym, err := purego.Dlsym(lib, name)
		if err != nil {
			purego.Dlclose(lib)
			return nil, fmt.Errorf("load %s from %s: %w", name, nvmlLibrary, err)
		}
		purego.RegisterFunc(fn, sym)
	}
	if rc := initialize(); rc != nvmlSuccess {
		err := fmt.Errorf("nvmlInit_v2: %s", f.errorString(rc))
		purego.Dlclose(lib)
		return nil, err
	}
	nvml = &f
	return nvml, nil
}

// nvmlCreatableProfiles asks NVML which profiles each GPU can still create
// and at which placements, keyed by GPU address. This is the query behind
// nvidia-smi vgpu -c -v. It fails for a GPU not in mixed-size mode, so
// equal-size hosts keep the sysfs lists.
func nvmlCreatableProfiles(gpus []string) (map[string][]creatableProfile, error) {
	f, err := openNVML()
	if err != nil {
		return nil, err
	}
	byGPU := make(map[string][]creatableProfile, len(gpus))
	for _, gpu := range gpus {
		profiles, err := f.creatableProfiles(gpu)
		if err != nil {
			return nil, fmt.Errorf("GPU %s: %w", gpu, err)
		}
		byGPU[gpu] = profiles
	}
	return byGPU, nil
}

func (f *nvmlFuncs) creatableProfiles(gpu string) ([]creatableProfile, error) {
	var device uintptr
	// sysfs addresses have a four-digit domain; NVML bus IDs have eight.
	if err := f.check("nvmlDeviceGetHandleByPciBusId_v2", f.deviceGetHandleByPCIBusID("0000"+gpu, &device)); err != nil {
		return nil, err
	}
	mode := nvmlHeterogeneousMode{version: nvmlStructVersion(unsafe.Sizeof(nvmlHeterogeneousMode{}), 1)}
	if err := f.check("nvmlDeviceGetVgpuHeterogeneousMode", f.deviceGetHeterogeneousMode(device, &mode)); err != nil {
		return nil, err
	}
	if mode.mode != nvmlFeatureEnabled {
		return nil, fmt.Errorf("not in mixed-size vGPU mode")
	}
	typeIDs, err := f.supportedTypes(device)
	if err != nil {
		return nil, err
	}
	profiles := make([]creatableProfile, 0)
	for _, typeID := range typeIDs {
		placements, err := f.creatablePlacements(device, typeID)
		if err != nil {
			return nil, err
		}
		if len(placements) == 0 {
			continue
		}
		name, err := f.typeName(typeID)
		if err != nil {
			return nil, err
		}
		profiles = append(profiles, creatableProfile{
			VGPUProfileType: VGPUProfileType{
				TypeName:      strconv.FormatUint(uint64(typeID), 10),
				Name:          name,
				FramebufferMB: framebufferFromProfileName(name),
			},
			placements: placements,
		})
	}
	return profiles, nil
}

func (f *nvmlFuncs) supportedTypes(device uintptr) ([]uint32, error) {
	typeIDs := make([]uint32, 64)
	count := uint32(len(typeIDs))
	rc := f.deviceGetSupportedVGPUs(device, &count, &typeIDs[0])
	if rc == nvmlInsufficientSize {
		typeIDs = make([]uint32, count)
		rc = f.deviceGetSupportedVGPUs(device, &count, &typeIDs[0])
	}
	if err := f.check("nvmlDeviceGetSupportedVgpus", rc); err != nil {
		return nil, err
	}
	return typeIDs[:count], nil
}

func (f *nvmlFuncs) creatablePlacements(device uintptr, typeID uint32) ([]int, error) {
	ids := make([]uint32, 64)
	list, rc := f.placementList(device, typeID, ids)
	if rc == nvmlInsufficientSize {
		ids = make([]uint32, list.count)
		list, rc = f.placementList(device, typeID, ids)
	}
	if err := f.check("nvmlDeviceGetVgpuTypeCreatablePlacements", rc); err != nil {
		return nil, err
	}
	placements := make([]int, 0, list.count)
	for _, id := range ids[:list.count] {
		placements = append(placements, int(id))
	}
	return placements, nil
}

func (f *nvmlFuncs) placementList(device uintptr, typeID uint32, ids []uint32) (nvmlPlacementList, int32) {
	// The list carries a pointer to ids, so ids must not move while NVML
	// writes to it.
	var pinner runtime.Pinner
	pinner.Pin(&ids[0])
	defer pinner.Unpin()
	list := nvmlPlacementList{
		version:      nvmlStructVersion(unsafe.Sizeof(nvmlPlacementList{}), 2),
		count:        uint32(len(ids)),
		placementIDs: &ids[0],
	}
	rc := f.deviceGetCreatablePlacements(device, typeID, &list)
	return list, rc
}

func (f *nvmlFuncs) typeName(typeID uint32) (string, error) {
	name := make([]byte, 96)
	size := uint32(len(name))
	if err := f.check("nvmlVgpuTypeGetName", f.vgpuTypeGetName(typeID, &name[0], &size)); err != nil {
		return "", err
	}
	value, _, _ := strings.Cut(string(name), "\x00")
	return value, nil
}

func (f *nvmlFuncs) check(call string, rc int32) error {
	if rc == nvmlSuccess {
		return nil
	}
	return fmt.Errorf("%s: %s", call, f.errorString(rc))
}
