//go:build linux

package devices

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/kernel/hypeman/lib/logger"
)

// In mixed-size (heterogeneous) mode each vGPU occupies a fixed run of its
// GPU's placement slots, and each profile may only start at the driver's
// heterogeneous placement IDs. The driver puts a new vGPU at the lowest free
// placement for its type, which scatters small profiles across the runs larger
// profiles need: on an L40S, half of the 2Q placements overlap a 4Q placement.
// After a type is configured, packPlacement moves the vGPU to the free
// placement that blocks the fewest placements of larger profiles. Packing is
// best effort: any failure keeps the driver's placement.

// vgpuPlacementTTL bounds how long a GPU's mode and geometry are reused, so a
// mode change made while hypeman runs is picked up.
const vgpuPlacementTTL = 10 * time.Minute

const nvidiaSMITimeout = 5 * time.Second

type vgpuPlacementType struct {
	size int
	ids  []int
}

// vgpuPlacementGeometry holds a GPU's placement table, which is empty unless
// the GPU is in mixed-size mode.
type vgpuPlacementGeometry struct {
	types    map[string]vgpuPlacementType // keyed by decimal vGPU type ID
	loadedAt time.Time
}

// vgpuPlacements caches geometry by parent GPU sysfs path, failures included,
// so a host without nvidia-smi does not run it on every claim. Guarded by
// vendorVFIOMu.
var vgpuPlacements = make(map[string]vgpuPlacementGeometry)

type placementSpan struct {
	start, size int
}

func (s vendorVFIOSysfs) packPlacement(ctx context.Context, vfAddress, profileType string) {
	log := logger.FromContext(ctx)
	vfPath := filepath.Join(s.pciDevicesPath, vfAddress)
	target, err := os.Readlink(filepath.Join(vfPath, "physfn"))
	if err != nil {
		log.WarnContext(ctx, "keeping driver vGPU placement: read parent GPU", "vf", vfAddress, "error", err)
		return
	}
	parentGPU := filepath.Base(target)
	geometry := s.placementGeometry(ctx, parentGPU)
	requested, ok := geometry.types[profileType]
	if !ok {
		return
	}
	placementPath := filepath.Join(vfPath, "nvidia", "placement_id")
	current, err := readPlacementID(placementPath)
	if err != nil {
		log.WarnContext(ctx, "keeping driver vGPU placement: read placement", "vf", vfAddress, "error", err)
		return
	}
	occupied, err := s.occupiedPlacements(parentGPU, vfAddress, geometry)
	if err != nil {
		log.WarnContext(ctx, "keeping driver vGPU placement: read sibling placements", "vf", vfAddress, "error", err)
		return
	}
	best := choosePlacement(geometry.types, occupied, requested, current)
	if best == current {
		return
	}
	if err := s.writeFile(placementPath, []byte(strconv.Itoa(best)), 0200); err != nil {
		log.WarnContext(ctx, "driver refused vGPU placement; keeping its own", "vf", vfAddress, "from", current, "to", best, "error", err)
		return
	}
	if moved, err := readPlacementID(placementPath); err != nil || moved != best {
		log.WarnContext(ctx, "vGPU placement did not move as written", "vf", vfAddress, "want", best, "got", moved, "error", err)
		return
	}
	log.InfoContext(ctx, "packed vGPU placement", "vf", vfAddress, "profile_type", profileType, "from", current, "to", best)
}

// placementGeometry returns the cached placement table for a GPU, reloading
// it once it is older than vgpuPlacementTTL. A failed load is cached as an
// empty table, which disables packing on that GPU until the next reload.
// Callers must hold vendorVFIOMu.
func (s vendorVFIOSysfs) placementGeometry(ctx context.Context, parentGPU string) vgpuPlacementGeometry {
	key := filepath.Join(s.pciDevicesPath, parentGPU)
	if cached, ok := vgpuPlacements[key]; ok && time.Since(cached.loadedAt) < vgpuPlacementTTL {
		return cached
	}
	geometry, err := s.loadPlacementGeometry(ctx, parentGPU)
	if err != nil {
		logger.FromContext(ctx).WarnContext(ctx, "vGPU placement packing disabled for GPU", "gpu", parentGPU, "error", err)
	}
	geometry.loadedAt = time.Now()
	vgpuPlacements[key] = geometry
	return geometry
}

func (s vendorVFIOSysfs) loadPlacementGeometry(ctx context.Context, parentGPU string) (vgpuPlacementGeometry, error) {
	query, err := s.nvidiaSMI(ctx, "-q", "-i", parentGPU)
	if err != nil {
		return vgpuPlacementGeometry{}, fmt.Errorf("query GPU: %w", err)
	}
	heterogeneous, err := parseHeterogeneousMode(string(query))
	if err != nil {
		return vgpuPlacementGeometry{}, err
	}
	if !heterogeneous {
		return vgpuPlacementGeometry{}, errors.New("GPU is in equal-size vGPU mode")
	}
	supported, err := s.nvidiaSMI(ctx, "vgpu", "-s", "-v", "-i", parentGPU)
	if err != nil {
		return vgpuPlacementGeometry{}, fmt.Errorf("query supported vGPU types: %w", err)
	}
	types, err := parsePlacementTypes(string(supported))
	if err != nil {
		return vgpuPlacementGeometry{}, err
	}
	return vgpuPlacementGeometry{types: types}, nil
}

func (s vendorVFIOSysfs) nvidiaSMI(ctx context.Context, args ...string) ([]byte, error) {
	if s.nvidiaSMIFunc != nil {
		return s.nvidiaSMIFunc(ctx, args...)
	}
	ctx, cancel := context.WithTimeout(ctx, nvidiaSMITimeout)
	defer cancel()
	return exec.CommandContext(ctx, "nvidia-smi", args...).Output()
}

// occupiedPlacements returns the placement spans of every configured VF on
// parentGPU other than vfAddress. An unreadable or unknown sibling fails the
// lookup, since a placement chosen around it could not be trusted.
func (s vendorVFIOSysfs) occupiedPlacements(parentGPU, vfAddress string, geometry vgpuPlacementGeometry) ([]placementSpan, error) {
	entries, err := os.ReadDir(s.pciDevicesPath)
	if err != nil {
		return nil, fmt.Errorf("read PCI devices: %w", err)
	}
	var occupied []placementSpan
	for _, entry := range entries {
		if entry.Name() == vfAddress {
			continue
		}
		vfPath := filepath.Join(s.pciDevicesPath, entry.Name())
		target, err := os.Readlink(filepath.Join(vfPath, "physfn"))
		if err != nil || filepath.Base(target) != parentGPU {
			continue
		}
		nvidiaPath := filepath.Join(vfPath, "nvidia")
		if _, err := os.Stat(nvidiaPath); err != nil {
			continue
		}
		currentType, err := readCurrentVGPUType(filepath.Join(nvidiaPath, "current_vgpu_type"))
		if err != nil {
			return nil, fmt.Errorf("read current vGPU type for VF %s: %w", entry.Name(), err)
		}
		if currentType == "0" {
			continue
		}
		placed, ok := geometry.types[currentType]
		if !ok {
			return nil, fmt.Errorf("VF %s has vGPU type %s with no known placement size", entry.Name(), currentType)
		}
		start, err := readPlacementID(filepath.Join(nvidiaPath, "placement_id"))
		if err != nil {
			return nil, fmt.Errorf("read placement for VF %s: %w", entry.Name(), err)
		}
		occupied = append(occupied, placementSpan{start: start, size: placed.size})
	}
	return occupied, nil
}

// choosePlacement returns the free placement for requested that blocks the
// fewest free placements of larger profiles, comparing the smallest larger
// size first. Ties keep current, so the driver's placement is only overridden
// for a strictly better one.
func choosePlacement(types map[string]vgpuPlacementType, occupied []placementSpan, requested vgpuPlacementType, current int) int {
	free := func(span placementSpan) bool {
		for _, other := range occupied {
			if span.start < other.start+other.size && other.start < span.start+span.size {
				return false
			}
		}
		return true
	}
	freeBySize := make(map[int]map[int]struct{})
	for _, placementType := range types {
		if placementType.size <= requested.size {
			continue
		}
		for _, id := range placementType.ids {
			if !free(placementSpan{start: id, size: placementType.size}) {
				continue
			}
			if freeBySize[placementType.size] == nil {
				freeBySize[placementType.size] = make(map[int]struct{})
			}
			freeBySize[placementType.size][id] = struct{}{}
		}
	}
	sizes := make([]int, 0, len(freeBySize))
	for size := range freeBySize {
		sizes = append(sizes, size)
	}
	sort.Ints(sizes)
	blocked := func(start int) []int {
		counts := make([]int, len(sizes))
		for i, size := range sizes {
			for id := range freeBySize[size] {
				if start < id+size && id < start+requested.size {
					counts[i]++
				}
			}
		}
		return counts
	}

	best, bestBlocked := current, blocked(current)
	ids := append([]int(nil), requested.ids...)
	sort.Ints(ids)
	for _, id := range ids {
		if !free(placementSpan{start: id, size: requested.size}) {
			continue
		}
		if candidate := blocked(id); fewerBlocked(candidate, bestBlocked) {
			best, bestBlocked = id, candidate
		}
	}
	return best
}

func fewerBlocked(a, b []int) bool {
	for i := range a {
		if a[i] != b[i] {
			return a[i] < b[i]
		}
	}
	return false
}

// parseHeterogeneousMode reads "vGPU Heterogeneous Mode : Enabled" from
// nvidia-smi -q output for a single GPU.
func parseHeterogeneousMode(output string) (bool, error) {
	for _, line := range strings.Split(output, "\n") {
		key, value, found := strings.Cut(line, ":")
		if !found || strings.TrimSpace(key) != "vGPU Heterogeneous Mode" {
			continue
		}
		switch strings.TrimSpace(value) {
		case "Enabled":
			return true, nil
		case "Disabled":
			return false, nil
		}
		return false, fmt.Errorf("unrecognized vGPU heterogeneous mode %q", strings.TrimSpace(value))
	}
	return false, fmt.Errorf("vGPU heterogeneous mode not reported")
}

// parsePlacementTypes reads each type's placement size and heterogeneous
// placement IDs from nvidia-smi vgpu -s -v output, keyed by decimal type ID
// to match current_vgpu_type. Types without numeric placement information
// are left out.
func parsePlacementTypes(output string) (map[string]vgpuPlacementType, error) {
	types := make(map[string]vgpuPlacementType)
	typeID := ""
	var current vgpuPlacementType
	flush := func() {
		if typeID != "" && current.size > 0 && len(current.ids) > 0 {
			types[typeID] = current
		}
	}
	for _, line := range strings.Split(output, "\n") {
		key, value, found := strings.Cut(line, ":")
		if !found {
			continue
		}
		key, value = strings.TrimSpace(key), strings.TrimSpace(value)
		switch key {
		case "vGPU Type ID":
			flush()
			id, err := strconv.ParseInt(value, 0, 64)
			if err != nil {
				return nil, fmt.Errorf("parse vGPU type ID %q: %w", value, err)
			}
			typeID, current = strconv.FormatInt(id, 10), vgpuPlacementType{}
		case "Placement Size":
			if size, err := strconv.Atoi(value); err == nil {
				current.size = size
			}
		case "Heterogeneous Placement IDs":
			for _, field := range strings.Fields(value) {
				id, err := strconv.Atoi(field)
				if err != nil {
					current.ids = nil
					break
				}
				current.ids = append(current.ids, id)
			}
		}
	}
	flush()
	if len(types) == 0 {
		return nil, fmt.Errorf("no vGPU placement information reported")
	}
	return types, nil
}

func readPlacementID(path string) (int, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	value := strings.TrimSpace(string(data))
	id, err := strconv.Atoi(value)
	if err != nil {
		return 0, fmt.Errorf("invalid placement ID %q", value)
	}
	return id, nil
}
