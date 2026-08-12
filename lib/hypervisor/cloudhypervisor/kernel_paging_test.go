package cloudhypervisor

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPrepareSnapshotForKernelPaging(t *testing.T) {
	t.Parallel()

	dir := writeKernelPagingSnapshot(t, kernelPagingSnapshotFixture{
		Ranges: []snapshotMemoryRange{
			{GPA: 0, Length: 4096},
			{GPA: 1 << 32, Length: 8192},
		},
		Mappings: []snapshotGuestRAMMapping{
			{Slot: 1, GPA: 1 << 32, Size: 8192, ZoneID: kernelPagingMemoryZoneID},
			{Slot: 0, GPA: 0, Size: 4096, ZoneID: kernelPagingMemoryZoneID},
		},
		MemorySize: 0,
		Zones: []map[string]any{
			{"id": kernelPagingMemoryZoneID, "size": 12288, "file": "/old/memory.raw"},
		},
	})

	optimized, err := prepareSnapshotForKernelPaging(dir)
	require.NoError(t, err)
	require.True(t, optimized)

	configData, err := os.ReadFile(filepath.Join(dir, cloudHypervisorConfigFile))
	require.NoError(t, err)
	var config struct {
		Memory struct {
			Size  uint64 `json:"size"`
			Zones []struct {
				ID       string `json:"id"`
				Size     uint64 `json:"size"`
				File     string `json:"file"`
				Shared   bool   `json:"shared"`
				Prefault bool   `json:"prefault"`
			} `json:"zones"`
		} `json:"memory"`
	}
	require.NoError(t, json.Unmarshal(configData, &config))
	assert.Zero(t, config.Memory.Size)
	require.Len(t, config.Memory.Zones, 1)
	assert.Equal(t, kernelPagingMemoryZoneID, config.Memory.Zones[0].ID)
	assert.Equal(t, uint64(12288), config.Memory.Zones[0].Size)
	assert.Equal(t, filepath.Join(dir, cloudHypervisorMemoryFile), config.Memory.Zones[0].File)
	assert.False(t, config.Memory.Zones[0].Shared)
	assert.False(t, config.Memory.Zones[0].Prefault)

	stateData, err := os.ReadFile(filepath.Join(dir, cloudHypervisorStateFile))
	require.NoError(t, err)
	_, _, ranges, mappings, err := decodeSnapshotMemoryState(stateData)
	require.NoError(t, err)
	assert.Empty(t, ranges)
	require.Len(t, mappings, 2)
	assert.Equal(t, uint64(4096), mappings[0].FileOffset)
	assert.Zero(t, mappings[1].FileOffset)
	assert.Contains(t, string(stateData), "18446744073709551615")

	optimized, err = prepareSnapshotForKernelPaging(dir)
	require.NoError(t, err)
	assert.False(t, optimized)
}

func TestPrepareSnapshotForKernelPagingWithHotplugOverlay(t *testing.T) {
	t.Setenv(experimentalHotplugOverlayEnv, "true")

	dir := writeKernelPagingSnapshot(t, kernelPagingSnapshotFixture{
		Ranges: []snapshotMemoryRange{
			{GPA: 0, Length: 4096},
			{GPA: 1 << 32, Length: 4096},
		},
		Mappings: []snapshotGuestRAMMapping{
			{Slot: 0, GPA: 0, Size: 4096, ZoneID: kernelPagingMemoryZoneID},
			{Slot: 1, GPA: 1 << 32, Size: 8192, ZoneID: kernelPagingMemoryZoneID, VirtioMem: true, FileOffset: 4096},
		},
		MemorySize:     0,
		MemoryFileSize: 12288,
		Zones: []map[string]any{{
			"id":              kernelPagingMemoryZoneID,
			"size":            4096,
			"file":            "/old/memory.raw",
			"hotplug_size":    8192,
			"hotplugged_size": 4096,
		}},
	})

	optimized, err := prepareSnapshotForKernelPaging(dir)
	require.NoError(t, err)
	require.True(t, optimized)

	stateData, err := os.ReadFile(filepath.Join(dir, cloudHypervisorStateFile))
	require.NoError(t, err)
	_, _, ranges, mappings, err := decodeSnapshotMemoryState(stateData)
	require.NoError(t, err)
	assert.Empty(t, ranges)
	require.Len(t, mappings, 2)
	assert.Equal(t, uint64(4096), mappings[1].FileOffset)

	configData, err := os.ReadFile(filepath.Join(dir, cloudHypervisorConfigFile))
	require.NoError(t, err)
	var config struct {
		Memory struct {
			Zones []struct {
				File           string `json:"file"`
				HotplugSize    uint64 `json:"hotplug_size"`
				HotpluggedSize uint64 `json:"hotplugged_size"`
			} `json:"zones"`
		} `json:"memory"`
	}
	require.NoError(t, json.Unmarshal(configData, &config))
	require.Len(t, config.Memory.Zones, 1)
	assert.Equal(t, filepath.Join(dir, cloudHypervisorMemoryFile), config.Memory.Zones[0].File)
	assert.Equal(t, uint64(8192), config.Memory.Zones[0].HotplugSize)
	assert.Equal(t, uint64(4096), config.Memory.Zones[0].HotpluggedSize)
}

func TestPrepareSnapshotForKernelPagingIgnoresUnmarkedMemoryZones(t *testing.T) {
	t.Parallel()

	dir := writeKernelPagingSnapshot(t, kernelPagingSnapshotFixture{
		Ranges:     []snapshotMemoryRange{{GPA: 0, Length: 4096}},
		Mappings:   []snapshotGuestRAMMapping{{Slot: 0, GPA: 0, Size: 4096, ZoneID: "custom"}},
		MemorySize: 0,
		Zones:      []map[string]any{{"id": "custom", "size": 4096}},
	})
	require.NoError(t, os.Remove(filepath.Join(dir, cloudHypervisorMemoryFile)))

	optimized, err := prepareSnapshotForKernelPaging(dir)
	require.NoError(t, err)
	assert.False(t, optimized)
}

func TestPrepareSnapshotForKernelPagingLeavesUnsupportedSnapshotsUnchanged(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		fixture kernelPagingSnapshotFixture
	}{
		{
			name: "implicit memory layout",
			fixture: kernelPagingSnapshotFixture{
				Ranges:     []snapshotMemoryRange{{GPA: 0, Length: 4096}},
				Mappings:   []snapshotGuestRAMMapping{{GPA: 0, Size: 4096, ZoneID: "mem0"}},
				MemorySize: 4096,
			},
		},
		{
			name: "balloon",
			fixture: kernelPagingSnapshotFixture{
				Ranges:     []snapshotMemoryRange{{GPA: 0, Length: 4096}},
				Mappings:   []snapshotGuestRAMMapping{{GPA: 0, Size: 4096, ZoneID: kernelPagingMemoryZoneID}},
				MemorySize: 0,
				Zones:      []map[string]any{{"id": kernelPagingMemoryZoneID, "size": 4096}},
				Balloon:    map[string]any{"size": 0},
			},
		},
		{
			name: "hugepages",
			fixture: kernelPagingSnapshotFixture{
				Ranges:     []snapshotMemoryRange{{GPA: 0, Length: 4096}},
				Mappings:   []snapshotGuestRAMMapping{{GPA: 0, Size: 4096, ZoneID: kernelPagingMemoryZoneID}},
				MemorySize: 0,
				Zones:      []map[string]any{{"id": kernelPagingMemoryZoneID, "size": 4096}},
				Hugepages:  true,
			},
		},
		{
			name: "vhost-user network",
			fixture: kernelPagingSnapshotFixture{
				Ranges:     []snapshotMemoryRange{{GPA: 0, Length: 4096}},
				Mappings:   []snapshotGuestRAMMapping{{GPA: 0, Size: 4096, ZoneID: kernelPagingMemoryZoneID}},
				MemorySize: 0,
				Zones:      []map[string]any{{"id": kernelPagingMemoryZoneID, "size": 4096}},
				Net:        []map[string]any{{"vhost_user": true}},
			},
		},
		{
			name: "PCI passthrough",
			fixture: kernelPagingSnapshotFixture{
				Ranges:     []snapshotMemoryRange{{GPA: 0, Length: 4096}},
				Mappings:   []snapshotGuestRAMMapping{{GPA: 0, Size: 4096, ZoneID: kernelPagingMemoryZoneID}},
				MemorySize: 0,
				Zones:      []map[string]any{{"id": kernelPagingMemoryZoneID, "size": 4096}},
				Devices:    []map[string]any{{"path": "/sys/bus/pci/devices/0000:00:00.0"}},
			},
		},
		{
			name: "partial virtio-mem range",
			fixture: kernelPagingSnapshotFixture{
				Ranges:     []snapshotMemoryRange{{GPA: 0, Length: 4096}},
				Mappings:   []snapshotGuestRAMMapping{{GPA: 0, Size: 8192, ZoneID: kernelPagingMemoryZoneID, VirtioMem: true}},
				MemorySize: 0,
				Zones:      []map[string]any{{"id": kernelPagingMemoryZoneID, "size": 8192}},
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := writeKernelPagingSnapshot(t, tc.fixture)
			configPath := filepath.Join(dir, cloudHypervisorConfigFile)
			statePath := filepath.Join(dir, cloudHypervisorStateFile)
			configBefore, err := os.ReadFile(configPath)
			require.NoError(t, err)
			stateBefore, err := os.ReadFile(statePath)
			require.NoError(t, err)

			optimized, err := prepareSnapshotForKernelPaging(dir)
			require.NoError(t, err)
			assert.False(t, optimized)
			configAfter, err := os.ReadFile(configPath)
			require.NoError(t, err)
			stateAfter, err := os.ReadFile(statePath)
			require.NoError(t, err)
			assert.Equal(t, configBefore, configAfter)
			assert.Equal(t, stateBefore, stateAfter)
		})
	}
}

type kernelPagingSnapshotFixture struct {
	Ranges         []snapshotMemoryRange
	Mappings       []snapshotGuestRAMMapping
	MemorySize     uint64
	MemoryFileSize int64
	Zones          []map[string]any
	Balloon        any
	Hugepages      bool
	Net            any
	Devices        any
}

func writeKernelPagingSnapshot(t *testing.T, fixture kernelPagingSnapshotFixture) string {
	t.Helper()

	dir := t.TempDir()
	memorySize := fixture.MemoryFileSize
	if memorySize == 0 {
		for _, r := range fixture.Ranges {
			memorySize += int64(r.Length)
		}
	}
	require.NoError(t, os.WriteFile(filepath.Join(dir, cloudHypervisorMemoryFile), nil, 0600))
	require.NoError(t, os.Truncate(filepath.Join(dir, cloudHypervisorMemoryFile), memorySize))

	memory := map[string]any{
		"size":            fixture.MemorySize,
		"mergeable":       false,
		"hotplug_method":  "Acpi",
		"hotplug_size":    nil,
		"hotplugged_size": nil,
		"shared":          false,
		"hugepages":       fixture.Hugepages,
		"hugepage_size":   nil,
		"prefault":        false,
		"zones":           fixture.Zones,
		"thp":             true,
	}
	config := map[string]any{
		"memory":       memory,
		"balloon":      fixture.Balloon,
		"devices":      fixture.Devices,
		"user_devices": nil,
		"vdpa":         nil,
		"fs":           nil,
		"disks":        []any{},
		"net":          fixture.Net,
	}
	configData, err := json.Marshal(config)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, cloudHypervisorConfigFile), configData, 0600))

	memoryState := map[string]any{
		"memory_ranges":      snapshotMemoryRanges{Data: fixture.Ranges},
		"guest_ram_mappings": fixture.Mappings,
		"arch_mem_regions": []any{
			map[string]any{"base": uint64(0), "size": ^uint64(0), "r_type": "Ram"},
		},
	}
	memoryStateData, err := json.Marshal(memoryState)
	require.NoError(t, err)
	state := map[string]any{
		"snapshots": map[string]any{
			cloudHypervisorMemoryID: map[string]any{
				"snapshots":     map[string]any{},
				"snapshot_data": map[string]any{"state": string(memoryStateData)},
			},
		},
	}
	stateData, err := json.Marshal(state)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, cloudHypervisorStateFile), stateData, 0600))
	return dir
}
