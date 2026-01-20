package devices

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"github.com/google/uuid"
	"github.com/kernel/hypeman/lib/logger"
)

const (
	mdevBusPath = "/sys/class/mdev_bus"
	mdevDevices = "/sys/bus/mdev/devices"
)

// mdevMu protects mdev creation/destruction to prevent race conditions
// when multiple instances request vGPUs concurrently.
var mdevMu sync.Mutex

// profileMetadata holds static profile info (doesn't change after driver load)
type profileMetadata struct {
	TypeName      string // e.g., "nvidia-1145"
	Name          string // e.g., "NVIDIA L40S-1B"
	FramebufferMB int
}

// cachedProfiles holds static profile metadata, loaded once on first access
var (
	cachedProfiles     []profileMetadata
	cachedProfilesOnce sync.Once
)

// DiscoverVFs returns all SR-IOV Virtual Functions available for vGPU.
// These are discovered by scanning /sys/class/mdev_bus/ which contains
// VFs that can host mdev devices.
func DiscoverVFs() ([]VirtualFunction, error) {
	entries, err := os.ReadDir(mdevBusPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil // No mdev_bus means no vGPU support
		}
		return nil, fmt.Errorf("read mdev_bus: %w", err)
	}

	// List mdevs once and build a lookup map to avoid O(n*m) performance
	mdevs, _ := ListMdevDevices()
	mdevByVF := make(map[string]bool, len(mdevs))
	for _, mdev := range mdevs {
		mdevByVF[mdev.VFAddress] = true
	}

	var vfs []VirtualFunction
	for _, entry := range entries {
		vfAddr := entry.Name()

		// Find parent GPU by checking physfn symlink
		// VFs have a physfn symlink pointing to their parent Physical Function
		physfnPath := filepath.Join("/sys/bus/pci/devices", vfAddr, "physfn")
		parentGPU := ""
		if target, err := os.Readlink(physfnPath); err == nil {
			parentGPU = filepath.Base(target)
		}

		// Check if this VF already has an mdev (using pre-built lookup map)
		hasMdev := mdevByVF[vfAddr]

		vfs = append(vfs, VirtualFunction{
			PCIAddress: vfAddr,
			ParentGPU:  parentGPU,
			HasMdev:    hasMdev,
		})
	}

	return vfs, nil
}

// ListGPUProfiles returns available vGPU profiles with availability counts.
// Profiles are discovered from the first VF's mdev_supported_types directory.
func ListGPUProfiles() ([]GPUProfile, error) {
	vfs, err := DiscoverVFs()
	if err != nil {
		return nil, err
	}
	return ListGPUProfilesWithVFs(vfs)
}

// ListGPUProfilesWithVFs returns available vGPU profiles using pre-discovered VFs.
// This avoids redundant VF discovery when the caller already has the list.
// Uses parallel sysfs reads for fast availability counting.
func ListGPUProfilesWithVFs(vfs []VirtualFunction) ([]GPUProfile, error) {
	if len(vfs) == 0 {
		return nil, nil
	}

	// Load static profile metadata once (cached indefinitely)
	cachedProfilesOnce.Do(func() {
		cachedProfiles = loadProfileMetadata(vfs[0].PCIAddress)
	})

	// Count availability for all profiles in parallel
	availability := countAvailableVFsForProfilesParallel(vfs, cachedProfiles)

	// Build result with dynamic availability counts
	profiles := make([]GPUProfile, 0, len(cachedProfiles))
	for _, meta := range cachedProfiles {
		profiles = append(profiles, GPUProfile{
			Name:          meta.Name,
			FramebufferMB: meta.FramebufferMB,
			Available:     availability[meta.TypeName],
		})
	}

	return profiles, nil
}

// loadProfileMetadata reads static profile info from sysfs (called once)
func loadProfileMetadata(firstVF string) []profileMetadata {
	typesPath := filepath.Join(mdevBusPath, firstVF, "mdev_supported_types")
	entries, err := os.ReadDir(typesPath)
	if err != nil {
		return nil
	}

	var profiles []profileMetadata
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}

		typeName := entry.Name()
		typeDir := filepath.Join(typesPath, typeName)

		nameBytes, err := os.ReadFile(filepath.Join(typeDir, "name"))
		if err != nil {
			continue
		}

		profiles = append(profiles, profileMetadata{
			TypeName:      typeName,
			Name:          strings.TrimSpace(string(nameBytes)),
			FramebufferMB: parseFramebufferFromDescription(typeDir),
		})
	}

	return profiles
}

// parseFramebufferFromDescription extracts framebuffer size from profile description
func parseFramebufferFromDescription(typeDir string) int {
	descBytes, err := os.ReadFile(filepath.Join(typeDir, "description"))
	if err != nil {
		return 0
	}

	// Description format varies but typically contains "framebuffer=1024M" or similar
	desc := string(descBytes)

	// Try to find framebuffer size in MB
	re := regexp.MustCompile(`framebuffer=(\d+)M`)
	if matches := re.FindStringSubmatch(desc); len(matches) > 1 {
		if mb, err := strconv.Atoi(matches[1]); err == nil {
			return mb
		}
	}

	// Also try comma-separated format like "num_heads=4, frl_config=60, framebuffer=1024M"
	scanner := bufio.NewScanner(strings.NewReader(desc))
	for scanner.Scan() {
		line := scanner.Text()
		if strings.Contains(line, "framebuffer") {
			parts := strings.Split(line, ",")
			for _, part := range parts {
				part = strings.TrimSpace(part)
				if strings.HasPrefix(part, "framebuffer=") {
					sizeStr := strings.TrimPrefix(part, "framebuffer=")
					sizeStr = strings.TrimSuffix(sizeStr, "M")
					if mb, err := strconv.Atoi(sizeStr); err == nil {
						return mb
					}
				}
			}
		}
	}

	return 0
}

// countAvailableVFsForProfilesParallel counts available instances for all profiles in parallel.
// Optimized: all VFs on the same parent GPU have identical profile support,
// so we only sample one VF per parent instead of reading from every VF.
func countAvailableVFsForProfilesParallel(vfs []VirtualFunction, profiles []profileMetadata) map[string]int {
	if len(vfs) == 0 || len(profiles) == 0 {
		return make(map[string]int)
	}

	// Group free VFs by parent GPU (done once, shared by all goroutines)
	freeVFsByParent := make(map[string][]VirtualFunction)
	for _, vf := range vfs {
		if vf.HasMdev {
			continue
		}
		freeVFsByParent[vf.ParentGPU] = append(freeVFsByParent[vf.ParentGPU], vf)
	}

	// Count availability for all profiles in parallel
	results := make(map[string]int, len(profiles))
	var mu sync.Mutex
	var wg sync.WaitGroup

	for _, meta := range profiles {
		wg.Add(1)
		go func(profileType string) {
			defer wg.Done()
			count := countAvailableForSingleProfile(freeVFsByParent, profileType)
			mu.Lock()
			results[profileType] = count
			mu.Unlock()
		}(meta.TypeName)
	}

	wg.Wait()
	return results
}

// countAvailableForSingleProfile counts available VFs for a single profile type.
// For SR-IOV vGPU (e.g., L40S), each VF has its own available_instances (typically 0 or 1).
// For time-sliced vGPU, available_instances may reflect shared GPU resources.
// We sum across all free VFs to handle both cases correctly.
func countAvailableForSingleProfile(freeVFsByParent map[string][]VirtualFunction, profileType string) int {
	count := 0
	for _, parentVFs := range freeVFsByParent {
		// Sum available_instances from all free VFs on this parent
		for _, vf := range parentVFs {
			availPath := filepath.Join(mdevBusPath, vf.PCIAddress, "mdev_supported_types", profileType, "available_instances")
			data, err := os.ReadFile(availPath)
			if err != nil {
				continue
			}
			instances, err := strconv.Atoi(strings.TrimSpace(string(data)))
			if err != nil {
				continue
			}
			count += instances
		}
	}
	return count
}

// findProfileType finds the internal type name (e.g., "nvidia-556") for a profile name (e.g., "L40S-1Q")
func findProfileType(profileName string) (string, error) {
	vfs, err := DiscoverVFs()
	if err != nil || len(vfs) == 0 {
		return "", fmt.Errorf("no VFs available")
	}

	firstVF := vfs[0].PCIAddress
	typesPath := filepath.Join(mdevBusPath, firstVF, "mdev_supported_types")
	entries, err := os.ReadDir(typesPath)
	if err != nil {
		return "", fmt.Errorf("read mdev_supported_types: %w", err)
	}

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		typeName := entry.Name()
		nameBytes, err := os.ReadFile(filepath.Join(typesPath, typeName, "name"))
		if err != nil {
			continue
		}
		if strings.TrimSpace(string(nameBytes)) == profileName {
			return typeName, nil
		}
	}

	return "", fmt.Errorf("profile %q not found", profileName)
}

// ListMdevDevices returns all active mdev devices on the host.
// Scans sysfs directly for fast, consistent results without external process overhead.
func ListMdevDevices() ([]MdevDevice, error) {
	entries, err := os.ReadDir(mdevDevices)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read mdev devices: %w", err)
	}

	var mdevs []MdevDevice
	for _, entry := range entries {
		uuid := entry.Name()
		mdevPath := filepath.Join(mdevDevices, uuid)

		// Read mdev_type symlink to get profile type
		typeLink, err := os.Readlink(filepath.Join(mdevPath, "mdev_type"))
		if err != nil {
			continue
		}
		profileType := filepath.Base(typeLink)

		// Get parent VF from symlink
		parentLink, err := os.Readlink(mdevPath)
		if err != nil {
			continue
		}
		// Parent path looks like ../../../devices/pci.../0000:82:00.4/uuid
		parts := strings.Split(parentLink, "/")
		vfAddress := ""
		for i, p := range parts {
			if strings.HasPrefix(p, "0000:") && i+1 < len(parts) && parts[i+1] == uuid {
				vfAddress = p
				break
			}
		}

		profileName := getProfileNameFromType(profileType, vfAddress)

		mdevs = append(mdevs, MdevDevice{
			UUID:        uuid,
			VFAddress:   vfAddress,
			ProfileType: profileType,
			ProfileName: profileName,
			SysfsPath:   mdevPath,
			InstanceID:  "",
		})
	}

	return mdevs, nil
}

// getProfileNameFromType resolves internal type (nvidia-556) to profile name (L40S-1Q)
func getProfileNameFromType(profileType, vfAddress string) string {
	if vfAddress == "" {
		return profileType // Fallback to type if no VF
	}

	namePath := filepath.Join(mdevBusPath, vfAddress, "mdev_supported_types", profileType, "name")
	data, err := os.ReadFile(namePath)
	if err != nil {
		return profileType
	}
	return strings.TrimSpace(string(data))
}

// CreateMdev creates an mdev device for the given profile and instance.
// It finds an available VF and creates the mdev, returning the device info.
// This function is thread-safe and uses a mutex to prevent race conditions
// when multiple instances request vGPUs concurrently.
func CreateMdev(ctx context.Context, profileName, instanceID string) (*MdevDevice, error) {
	log := logger.FromContext(ctx)

	// Lock to prevent race conditions when multiple instances request the same profile
	mdevMu.Lock()
	defer mdevMu.Unlock()

	// Find profile type from name
	profileType, err := findProfileType(profileName)
	if err != nil {
		return nil, err
	}

	// Find an available VF
	vfs, err := DiscoverVFs()
	if err != nil {
		return nil, fmt.Errorf("discover VFs: %w", err)
	}

	var targetVF string
	for _, vf := range vfs {
		// Skip VFs that already have an mdev
		if vf.HasMdev {
			continue
		}
		// Check if this VF can create the profile
		availPath := filepath.Join(mdevBusPath, vf.PCIAddress, "mdev_supported_types", profileType, "available_instances")
		data, err := os.ReadFile(availPath)
		if err != nil {
			continue
		}
		instances, err := strconv.Atoi(strings.TrimSpace(string(data)))
		if err != nil || instances < 1 {
			continue
		}
		targetVF = vf.PCIAddress
		break
	}

	if targetVF == "" {
		return nil, fmt.Errorf("no available VF for profile %q", profileName)
	}

	// Generate UUID for the mdev
	mdevUUID := uuid.New().String()

	log.DebugContext(ctx, "creating mdev device", "profile", profileName, "vf", targetVF, "uuid", mdevUUID, "instance_id", instanceID)

	// Create mdev by writing UUID to create file
	createPath := filepath.Join(mdevBusPath, targetVF, "mdev_supported_types", profileType, "create")
	if err := os.WriteFile(createPath, []byte(mdevUUID), 0200); err != nil {
		return nil, fmt.Errorf("create mdev on VF %s: %w", targetVF, err)
	}

	log.InfoContext(ctx, "created mdev device", "profile", profileName, "vf", targetVF, "uuid", mdevUUID, "instance_id", instanceID)

	return &MdevDevice{
		UUID:        mdevUUID,
		VFAddress:   targetVF,
		ProfileType: profileType,
		ProfileName: profileName,
		SysfsPath:   filepath.Join(mdevDevices, mdevUUID),
		InstanceID:  instanceID,
	}, nil
}

// DestroyMdev removes an mdev device.
func DestroyMdev(ctx context.Context, mdevUUID string) error {
	log := logger.FromContext(ctx)

	// Lock to prevent race conditions during destruction
	mdevMu.Lock()
	defer mdevMu.Unlock()

	log.DebugContext(ctx, "destroying mdev device", "uuid", mdevUUID)

	// Try mdevctl undefine first (removes persistent definition)
	if err := exec.Command("mdevctl", "undefine", "--uuid", mdevUUID).Run(); err != nil {
		// Log at debug level - mdevctl might not be installed or mdev might not be defined
		log.DebugContext(ctx, "mdevctl undefine failed (may be expected)", "uuid", mdevUUID, "error", err)
	}

	// Remove via sysfs
	removePath := filepath.Join(mdevDevices, mdevUUID, "remove")
	if err := os.WriteFile(removePath, []byte("1"), 0200); err != nil {
		if os.IsNotExist(err) {
			log.DebugContext(ctx, "mdev already removed", "uuid", mdevUUID)
			return nil // Already removed
		}
		return fmt.Errorf("remove mdev %s: %w", mdevUUID, err)
	}

	log.InfoContext(ctx, "destroyed mdev device", "uuid", mdevUUID)
	return nil
}

// IsMdevInUse checks if an mdev device is currently bound to a driver (in use by a VM).
// An mdev with a driver symlink is actively attached to a hypervisor/VFIO.
func IsMdevInUse(mdevUUID string) bool {
	driverPath := filepath.Join(mdevDevices, mdevUUID, "driver")
	_, err := os.Readlink(driverPath)
	return err == nil // Has a driver = in use
}

// MdevReconcileInfo contains information needed to reconcile mdevs for an instance
type MdevReconcileInfo struct {
	InstanceID string
	MdevUUID   string
	IsRunning  bool // true if instance's VMM is running or state is unknown
}

// ReconcileMdevs destroys orphaned mdevs that belong to hypeman but are no longer in use.
// This is called on server startup to clean up stale mdevs from previous runs.
//
// Safety guarantees:
//   - Only destroys mdevs that are tracked by hypeman instances (via hypemanMdevs map)
//   - Never destroys mdevs created by other processes on the host
//   - Skips mdevs that are currently bound to a driver (in use by a VM)
//   - Skips mdevs for instances in Running or Unknown state
func ReconcileMdevs(ctx context.Context, instanceInfos []MdevReconcileInfo) error {
	log := logger.FromContext(ctx)

	mdevs, err := ListMdevDevices()
	if err != nil {
		return fmt.Errorf("list mdevs: %w", err)
	}

	if len(mdevs) == 0 {
		log.DebugContext(ctx, "no mdev devices found to reconcile")
		return nil
	}

	// Build lookup maps from instance info
	// mdevUUID -> instanceID for mdevs managed by hypeman
	hypemanMdevs := make(map[string]string, len(instanceInfos))
	// instanceID -> isRunning for liveness check
	instanceRunning := make(map[string]bool, len(instanceInfos))
	for _, info := range instanceInfos {
		if info.MdevUUID != "" {
			hypemanMdevs[info.MdevUUID] = info.InstanceID
			instanceRunning[info.InstanceID] = info.IsRunning
		}
	}

	log.InfoContext(ctx, "reconciling mdev devices", "total_mdevs", len(mdevs), "hypeman_mdevs", len(hypemanMdevs))

	var destroyed, skippedNotOurs, skippedInUse, skippedRunning int
	for _, mdev := range mdevs {
		// Only consider mdevs that hypeman created
		instanceID, isOurs := hypemanMdevs[mdev.UUID]
		if !isOurs {
			log.DebugContext(ctx, "skipping mdev not managed by hypeman", "uuid", mdev.UUID, "profile", mdev.ProfileName)
			skippedNotOurs++
			continue
		}

		// Skip if instance is running or in unknown state (might still be using the mdev)
		if instanceRunning[instanceID] {
			log.DebugContext(ctx, "skipping mdev for running/unknown instance", "uuid", mdev.UUID, "instance_id", instanceID)
			skippedRunning++
			continue
		}

		// Check if mdev is bound to a driver (in use by VM)
		if IsMdevInUse(mdev.UUID) {
			log.WarnContext(ctx, "skipping mdev still bound to driver", "uuid", mdev.UUID, "instance_id", instanceID)
			skippedInUse++
			continue
		}

		// Safe to destroy - it's ours, instance is not running, and not bound to driver
		log.InfoContext(ctx, "destroying orphaned mdev", "uuid", mdev.UUID, "profile", mdev.ProfileName, "instance_id", instanceID)
		if err := DestroyMdev(ctx, mdev.UUID); err != nil {
			// Log error but continue - best effort cleanup
			log.WarnContext(ctx, "failed to destroy orphaned mdev", "uuid", mdev.UUID, "error", err)
			continue
		}
		destroyed++
	}

	log.InfoContext(ctx, "mdev reconciliation complete",
		"destroyed", destroyed,
		"skipped_not_ours", skippedNotOurs,
		"skipped_in_use", skippedInUse,
		"skipped_running", skippedRunning,
	)

	return nil
}
