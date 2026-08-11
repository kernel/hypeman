package cloudhypervisor

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

const (
	cloudHypervisorConfigFile = "config.json"
	cloudHypervisorStateFile  = "state.json"
	cloudHypervisorMemoryFile = "memory-ranges"
	cloudHypervisorMemoryID   = "memory-manager"
)

type snapshotMemoryRange struct {
	GPA    uint64 `json:"gpa"`
	Length uint64 `json:"length"`
}

type snapshotMemoryRanges struct {
	Data []snapshotMemoryRange `json:"data"`
}

type snapshotGuestRAMMapping struct {
	Slot       uint32 `json:"slot"`
	GPA        uint64 `json:"gpa"`
	Size       uint64 `json:"size"`
	ZoneID     string `json:"zone_id"`
	VirtioMem  bool   `json:"virtio_mem"`
	FileOffset uint64 `json:"file_offset"`
}

type snapshotRangeKey struct {
	gpa    uint64
	length uint64
}

// prepareSnapshotForKernelPaging changes an ordinary Cloud Hypervisor snapshot
// from eager-copy restore to a private file-backed restore. Cloud Hypervisor
// already supports MAP_PRIVATE memory zones; clearing the saved range table
// makes restore use that mapping directly instead of copying memory-ranges into
// anonymous RAM first. The snapshot memory file must remain immutable while a
// restored VM is running.
//
// Unsupported device/memory layouts are left untouched and use Cloud
// Hypervisor's normal eager restore path.
func prepareSnapshotForKernelPaging(snapshotDir string) (bool, error) {
	configPath := filepath.Join(snapshotDir, cloudHypervisorConfigFile)
	statePath := filepath.Join(snapshotDir, cloudHypervisorStateFile)
	memoryPath := filepath.Join(snapshotDir, cloudHypervisorMemoryFile)

	configData, err := os.ReadFile(configPath)
	if err != nil {
		return false, fmt.Errorf("read snapshot config: %w", err)
	}
	if _, eligible, err := kernelPagingConfig(configData, memoryPath, nil); err != nil {
		return false, err
	} else if !eligible {
		return false, nil
	}

	stateData, err := os.ReadFile(statePath)
	if err != nil {
		return false, fmt.Errorf("read snapshot state: %w", err)
	}
	memoryInfo, err := os.Stat(memoryPath)
	if err != nil {
		return false, fmt.Errorf("stat snapshot memory: %w", err)
	}

	stateFile, memoryState, ranges, mappings, err := decodeSnapshotMemoryState(stateData)
	if err != nil {
		return false, err
	}
	if len(ranges) == 0 {
		return false, nil
	}

	fileOffsets, totalSize, ok := snapshotMappingFileOffsets(ranges, mappings)
	if !ok || totalSize != uint64(memoryInfo.Size()) {
		return false, nil
	}
	for i := range mappings {
		mappings[i].FileOffset = fileOffsets[i]
	}

	updatedConfig, ok, err := kernelPagingConfig(configData, memoryPath, mappings)
	if err != nil {
		return false, err
	}
	if !ok {
		return false, nil
	}

	emptyRanges, err := json.Marshal(snapshotMemoryRanges{Data: make([]snapshotMemoryRange, 0)})
	if err != nil {
		return false, fmt.Errorf("marshal empty snapshot memory ranges: %w", err)
	}
	updatedMappings, err := json.Marshal(mappings)
	if err != nil {
		return false, fmt.Errorf("marshal snapshot memory mappings: %w", err)
	}
	memoryState["memory_ranges"] = emptyRanges
	memoryState["guest_ram_mappings"] = updatedMappings
	updatedState, err := encodeSnapshotMemoryState(stateFile, memoryState)
	if err != nil {
		return false, err
	}

	// Commit config first. If state replacement fails, the old range table
	// still drives an eager copy into the private mapping, which remains safe.
	// Replacing state last commits the lazy restore atomically.
	if err := replaceFile(configPath, updatedConfig); err != nil {
		return false, fmt.Errorf("replace snapshot config: %w", err)
	}
	if err := replaceFile(statePath, updatedState); err != nil {
		return false, fmt.Errorf("replace snapshot state: %w", err)
	}
	return true, nil
}

func decodeSnapshotMemoryState(stateData []byte) (
	map[string]json.RawMessage,
	map[string]json.RawMessage,
	[]snapshotMemoryRange,
	[]snapshotGuestRAMMapping,
	error,
) {
	var stateFile map[string]json.RawMessage
	if err := json.Unmarshal(stateData, &stateFile); err != nil {
		return nil, nil, nil, nil, fmt.Errorf("decode snapshot state: %w", err)
	}

	var snapshots map[string]json.RawMessage
	if err := json.Unmarshal(stateFile["snapshots"], &snapshots); err != nil {
		return nil, nil, nil, nil, fmt.Errorf("decode snapshot components: %w", err)
	}
	memoryComponent, ok := snapshots[cloudHypervisorMemoryID]
	if !ok {
		return nil, nil, nil, nil, errors.New("snapshot is missing memory-manager state")
	}

	var component map[string]json.RawMessage
	if err := json.Unmarshal(memoryComponent, &component); err != nil {
		return nil, nil, nil, nil, fmt.Errorf("decode memory-manager component: %w", err)
	}
	var snapshotData map[string]json.RawMessage
	if err := json.Unmarshal(component["snapshot_data"], &snapshotData); err != nil {
		return nil, nil, nil, nil, fmt.Errorf("decode memory-manager snapshot data: %w", err)
	}
	var encodedState string
	if err := json.Unmarshal(snapshotData["state"], &encodedState); err != nil {
		return nil, nil, nil, nil, fmt.Errorf("decode memory-manager state string: %w", err)
	}

	var memoryState map[string]json.RawMessage
	if err := json.Unmarshal([]byte(encodedState), &memoryState); err != nil {
		return nil, nil, nil, nil, fmt.Errorf("decode memory-manager state: %w", err)
	}
	var ranges snapshotMemoryRanges
	if err := json.Unmarshal(memoryState["memory_ranges"], &ranges); err != nil {
		return nil, nil, nil, nil, fmt.Errorf("decode snapshot memory ranges: %w", err)
	}
	var mappings []snapshotGuestRAMMapping
	if err := json.Unmarshal(memoryState["guest_ram_mappings"], &mappings); err != nil {
		return nil, nil, nil, nil, fmt.Errorf("decode guest RAM mappings: %w", err)
	}

	return stateFile, memoryState, ranges.Data, mappings, nil
}

func encodeSnapshotMemoryState(stateFile, memoryState map[string]json.RawMessage) ([]byte, error) {
	encodedMemoryState, err := json.Marshal(memoryState)
	if err != nil {
		return nil, fmt.Errorf("marshal memory-manager state: %w", err)
	}

	var snapshots map[string]json.RawMessage
	if err := json.Unmarshal(stateFile["snapshots"], &snapshots); err != nil {
		return nil, fmt.Errorf("decode snapshot components: %w", err)
	}
	var component map[string]json.RawMessage
	if err := json.Unmarshal(snapshots[cloudHypervisorMemoryID], &component); err != nil {
		return nil, fmt.Errorf("decode memory-manager component: %w", err)
	}
	var snapshotData map[string]json.RawMessage
	if err := json.Unmarshal(component["snapshot_data"], &snapshotData); err != nil {
		return nil, fmt.Errorf("decode memory-manager snapshot data: %w", err)
	}
	stateString, err := json.Marshal(string(encodedMemoryState))
	if err != nil {
		return nil, fmt.Errorf("marshal memory-manager state string: %w", err)
	}
	snapshotData["state"] = stateString
	component["snapshot_data"], err = json.Marshal(snapshotData)
	if err != nil {
		return nil, fmt.Errorf("marshal memory-manager snapshot data: %w", err)
	}
	snapshots[cloudHypervisorMemoryID], err = json.Marshal(component)
	if err != nil {
		return nil, fmt.Errorf("marshal memory-manager component: %w", err)
	}
	stateFile["snapshots"], err = json.Marshal(snapshots)
	if err != nil {
		return nil, fmt.Errorf("marshal snapshot components: %w", err)
	}
	updated, err := json.Marshal(stateFile)
	if err != nil {
		return nil, fmt.Errorf("marshal snapshot state: %w", err)
	}
	return updated, nil
}

func snapshotMappingFileOffsets(
	ranges []snapshotMemoryRange,
	mappings []snapshotGuestRAMMapping,
) ([]uint64, uint64, bool) {
	offsetsByRange := make(map[snapshotRangeKey]uint64, len(ranges))
	var offset uint64
	for _, r := range ranges {
		if r.Length == 0 || offset > ^uint64(0)-r.Length {
			return nil, 0, false
		}
		key := snapshotRangeKey{gpa: r.GPA, length: r.Length}
		if _, exists := offsetsByRange[key]; exists {
			return nil, 0, false
		}
		offsetsByRange[key] = offset
		offset += r.Length
	}
	if len(mappings) != len(ranges) {
		return nil, 0, false
	}

	mappingOffsets := make([]uint64, len(mappings))
	seen := make(map[snapshotRangeKey]struct{}, len(mappings))
	for i, mapping := range mappings {
		if mapping.VirtioMem || mapping.Size == 0 {
			return nil, 0, false
		}
		key := snapshotRangeKey{gpa: mapping.GPA, length: mapping.Size}
		fileOffset, exists := offsetsByRange[key]
		if !exists {
			return nil, 0, false
		}
		if _, duplicate := seen[key]; duplicate {
			return nil, 0, false
		}
		seen[key] = struct{}{}
		mappingOffsets[i] = fileOffset
	}
	return mappingOffsets, offset, true
}

func kernelPagingConfig(
	configData []byte,
	memoryPath string,
	mappings []snapshotGuestRAMMapping,
) ([]byte, bool, error) {
	var config map[string]json.RawMessage
	if err := json.Unmarshal(configData, &config); err != nil {
		return nil, false, fmt.Errorf("decode snapshot config: %w", err)
	}
	if hasConfiguredDevice(config["balloon"]) ||
		hasConfiguredDevice(config["devices"]) ||
		hasConfiguredDevice(config["user_devices"]) ||
		hasConfiguredDevice(config["vdpa"]) ||
		hasConfiguredDevice(config["fs"]) ||
		hasConfiguredDevice(config["ivshmem"]) {
		return nil, false, nil
	}
	if devicesUseVhostUser(config["disks"]) || devicesUseVhostUser(config["net"]) {
		return nil, false, nil
	}

	var memory map[string]json.RawMessage
	if err := json.Unmarshal(config["memory"], &memory); err != nil {
		return nil, false, fmt.Errorf("decode snapshot memory config: %w", err)
	}
	if rawBool(memory["shared"]) || rawBool(memory["hugepages"]) ||
		hasConfiguredDevice(memory["hotplug_size"]) || hasConfiguredDevice(memory["hotplugged_size"]) {
		return nil, false, nil
	}

	if !hasConfiguredDevice(memory["zones"]) {
		// Switching an existing VM from implicit memory to explicit zones
		// changes its platform topology. Only VMs booted with file-backed zones
		// can safely restore through this path.
		return nil, false, nil
	}
	var zones []map[string]json.RawMessage
	if err := json.Unmarshal(memory["zones"], &zones); err != nil {
		return nil, false, fmt.Errorf("decode snapshot memory zones: %w", err)
	}
	if len(zones) != 1 {
		return nil, false, nil
	}

	zone := zones[0]
	if rawBool(zone["shared"]) || rawBool(zone["hugepages"]) ||
		hasConfiguredDevice(zone["hotplug_size"]) || hasConfiguredDevice(zone["hotplugged_size"]) {
		return nil, false, nil
	}
	var zoneID string
	if err := json.Unmarshal(zone["id"], &zoneID); err != nil || zoneID != kernelPagingMemoryZoneID {
		return nil, false, nil
	}
	for _, mapping := range mappings {
		if mapping.ZoneID != zoneID {
			return nil, false, nil
		}
	}

	encodedMemoryPath, err := json.Marshal(memoryPath)
	if err != nil {
		return nil, false, fmt.Errorf("marshal snapshot memory path: %w", err)
	}
	zone["file"] = encodedMemoryPath
	zone["shared"] = json.RawMessage("false")
	zone["prefault"] = json.RawMessage("false")
	zonesData, err := json.Marshal(zones)
	if err != nil {
		return nil, false, fmt.Errorf("marshal snapshot memory zones: %w", err)
	}

	memory["size"] = json.RawMessage("0")
	memory["shared"] = json.RawMessage("false")
	memory["hugepages"] = json.RawMessage("false")
	memory["prefault"] = json.RawMessage("false")
	memory["hotplug_size"] = json.RawMessage("null")
	memory["hotplugged_size"] = json.RawMessage("null")
	memory["zones"] = zonesData
	updatedMemory, err := json.Marshal(memory)
	if err != nil {
		return nil, false, fmt.Errorf("marshal snapshot memory config: %w", err)
	}
	config["memory"] = updatedMemory
	updated, err := json.Marshal(config)
	if err != nil {
		return nil, false, fmt.Errorf("marshal snapshot config: %w", err)
	}
	return updated, true, nil
}

func hasConfiguredDevice(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	return len(trimmed) > 0 && !bytes.Equal(trimmed, []byte("null")) && !bytes.Equal(trimmed, []byte("[]"))
}

func rawBool(raw json.RawMessage) bool {
	var value bool
	return json.Unmarshal(raw, &value) == nil && value
}

func devicesUseVhostUser(raw json.RawMessage) bool {
	var devices []map[string]json.RawMessage
	if json.Unmarshal(raw, &devices) != nil {
		return true
	}
	for _, device := range devices {
		if rawBool(device["vhost_user"]) {
			return true
		}
	}
	return false
}

func replaceFile(path string, data []byte) (retErr error) {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer func() {
		if retErr != nil {
			_ = os.Remove(tmpPath)
		}
	}()
	if err := tmp.Chmod(info.Mode().Perm()); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
}
