package devices

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"github.com/google/uuid"
	"github.com/onkernel/hypeman/lib/logger"
)

const (
	mdevBusPath = "/sys/class/mdev_bus"
	mdevDevices = "/sys/bus/mdev/devices"
)

// mdevMu protects mdev creation/destruction to prevent race conditions
// when multiple instances request vGPUs concurrently.
var mdevMu sync.Mutex

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
	if len(vfs) == 0 {
		return nil, nil
	}

	// Get profile types from first VF
	firstVF := vfs[0].PCIAddress
	typesPath := filepath.Join(mdevBusPath, firstVF, "mdev_supported_types")
	entries, err := os.ReadDir(typesPath)
	if err != nil {
		return nil, fmt.Errorf("read mdev_supported_types: %w", err)
	}

	var profiles []GPUProfile
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}

		typeName := entry.Name() // e.g., "nvidia-556"
		typeDir := filepath.Join(typesPath, typeName)

		// Read profile name from 'name' file
		nameBytes, err := os.ReadFile(filepath.Join(typeDir, "name"))
		if err != nil {
			continue
		}
		profileName := strings.TrimSpace(string(nameBytes))

		// Parse framebuffer size from description file
		framebufferMB := parseFramebufferFromDescription(typeDir)

		// Count available VFs for this profile type
		available := countAvailableVFsForProfile(vfs, typeName)

		profiles = append(profiles, GPUProfile{
			Name:          profileName,
			FramebufferMB: framebufferMB,
			Available:     available,
		})
	}

	return profiles, nil
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

// countAvailableVFsForProfile counts VFs that can still create the given profile type
func countAvailableVFsForProfile(vfs []VirtualFunction, profileType string) int {
	count := 0
	for _, vf := range vfs {
		// Check available_instances for this profile on this VF
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

// mdevctlDevice represents the JSON structure from mdevctl list
type mdevctlDevice struct {
	Start        string `json:"start,omitempty"`
	MdevType     string `json:"mdev_type,omitempty"`
	ManuallyDef  bool   `json:"manually_defined,omitempty"`
	ParentDevice string `json:"parent,omitempty"`
}

// ListMdevDevices returns all active mdev devices on the host.
func ListMdevDevices() ([]MdevDevice, error) {
	// Try mdevctl first
	output, err := exec.Command("mdevctl", "list", "-d", "--dumpjson").Output()
	if err == nil && len(output) > 0 {
		return parseMdevctlOutput(output)
	}

	// Fallback to sysfs scanning
	return scanMdevDevices()
}

// parseMdevctlOutput parses the JSON output from mdevctl list
func parseMdevctlOutput(output []byte) ([]MdevDevice, error) {
	// mdevctl outputs: { "uuid": { ... }, "uuid2": { ... } }
	var rawMap map[string][]mdevctlDevice
	if err := json.Unmarshal(output, &rawMap); err != nil {
		return nil, fmt.Errorf("parse mdevctl output: %w", err)
	}

	var mdevs []MdevDevice
	for uuid, devices := range rawMap {
		if len(devices) == 0 {
			continue
		}
		dev := devices[0]

		// Get profile name from mdev type
		profileName := getProfileNameFromType(dev.MdevType, dev.ParentDevice)

		mdevs = append(mdevs, MdevDevice{
			UUID:        uuid,
			VFAddress:   dev.ParentDevice,
			ProfileType: dev.MdevType,
			ProfileName: profileName,
			SysfsPath:   filepath.Join(mdevDevices, uuid),
			InstanceID:  "", // Not tracked by mdevctl, we track separately
		})
	}

	return mdevs, nil
}

// scanMdevDevices scans /sys/bus/mdev/devices for active mdevs
func scanMdevDevices() ([]MdevDevice, error) {
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
