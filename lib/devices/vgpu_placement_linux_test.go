//go:build linux

package devices

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"github.com/kernel/hypeman/lib/logger"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	testL40S1Q = "1147"
	testL40S2Q = "1148"
	testL40S4Q = "1150"
)

const testHeterogeneousQuery = `GPU 00000000:82:00.0
    GPU Virtualization Mode
        Virtualization Mode                            : Host VGPU
        Host VGPU Mode                                 : SR-IOV
        vGPU Heterogeneous Mode                        : Enabled
`

func loadL40SPlacementTypes(t *testing.T) map[string]vgpuPlacementType {
	t.Helper()
	output, err := os.ReadFile(filepath.Join("testdata", "nvidia-smi-vgpu-supported-l40s.txt"))
	require.NoError(t, err)
	types, err := parsePlacementTypes(string(output))
	require.NoError(t, err)
	return types
}

func TestParsePlacementTypes(t *testing.T) {
	types := loadL40SPlacementTypes(t)

	assert.Equal(t, vgpuPlacementType{size: 2, ids: []int{0, 4, 6, 10, 12, 16, 18, 22, 24, 28, 30, 34, 36, 40, 42, 46}}, types[testL40S2Q])
	assert.Equal(t, vgpuPlacementType{size: 4, ids: []int{0, 8, 12, 20, 24, 32, 36, 44}}, types[testL40S4Q])
	assert.Equal(t, 1, types[testL40S1Q].size)
}

func TestParseHeterogeneousMode(t *testing.T) {
	enabled, err := parseHeterogeneousMode(testHeterogeneousQuery)
	require.NoError(t, err)
	assert.True(t, enabled)

	enabled, err = parseHeterogeneousMode(strings.Replace(testHeterogeneousQuery, "Enabled", "Disabled", 1))
	require.NoError(t, err)
	assert.False(t, enabled)

	_, err = parseHeterogeneousMode("GPU 00000000:82:00.0\n")
	require.Error(t, err)
}

func placementFree(occupied []placementSpan, span placementSpan) bool {
	for _, other := range occupied {
		if span.start < other.start+other.size && other.start < span.start+span.size {
			return false
		}
	}
	return true
}

// placeLikeDriver returns the lowest free placement for typeID, which is
// where the driver puts a newly configured vGPU.
func placeLikeDriver(types map[string]vgpuPlacementType, occupied []placementSpan, typeID string) (int, bool) {
	requested := types[typeID]
	for _, id := range requested.ids {
		if placementFree(occupied, placementSpan{start: id, size: requested.size}) {
			return id, true
		}
	}
	return 0, false
}

func free4QPlacements(types map[string]vgpuPlacementType, occupied []placementSpan) int {
	count := 0
	for _, id := range types[testL40S4Q].ids {
		if placementFree(occupied, placementSpan{start: id, size: 4}) {
			count++
		}
	}
	return count
}

func TestChoosePlacementKeeps4QRoomOnL40S(t *testing.T) {
	types := loadL40SPlacementTypes(t)

	var packed, unpacked []placementSpan
	for i := 0; i < 8; i++ {
		start, ok := placeLikeDriver(types, unpacked, testL40S2Q)
		require.True(t, ok)
		unpacked = append(unpacked, placementSpan{start: start, size: 2})

		start, ok = placeLikeDriver(types, packed, testL40S2Q)
		require.True(t, ok)
		start = choosePlacement(types, packed, types[testL40S2Q], start)
		packed = append(packed, placementSpan{start: start, size: 2})
	}

	starts := make([]int, 0, len(packed))
	for _, span := range packed {
		starts = append(starts, span.start)
	}
	sort.Ints(starts)
	assert.Equal(t, []int{4, 6, 16, 18, 28, 30, 40, 42}, starts, "the first eight 2Q fill the placements no 4Q overlaps")
	assert.Equal(t, 8, free4QPlacements(types, packed))
	assert.Equal(t, 4, free4QPlacements(types, unpacked), "the driver's lowest-free placement blocks half the 4Q room")

	// Once those are used, every further 2Q costs exactly one 4Q placement.
	for i := 0; i < 8; i++ {
		start, ok := placeLikeDriver(types, packed, testL40S2Q)
		require.True(t, ok)
		start = choosePlacement(types, packed, types[testL40S2Q], start)
		packed = append(packed, placementSpan{start: start, size: 2})
		assert.Equal(t, 7-i, free4QPlacements(types, packed))
	}
}

func TestChoosePlacementKeepsCurrentOnTie(t *testing.T) {
	types := loadL40SPlacementTypes(t)
	// 4Q at 0 and 8 fill the first two 4Q placements; 2Q at 4 and 6 are
	// equally good, so the driver's choice stands.
	occupied := []placementSpan{{start: 0, size: 4}, {start: 8, size: 4}}

	assert.Equal(t, 6, choosePlacement(types, occupied, types[testL40S2Q], 6))
	assert.Equal(t, 4, choosePlacement(types, occupied, types[testL40S2Q], 4))
}

// addPlacedVF adds a VF with a configured type at placement, as the driver
// leaves it after a type write.
func (s testVendorVFIOSysfs) addPlacedVF(t *testing.T, address, vfioID, currentType string, placement int) {
	t.Helper()
	s.addVF(t, "0000:82:00.0", address, vfioID, currentType, testCreatableTypes)
	require.NoError(t, os.WriteFile(filepath.Join(s.pciDevicesPath, address, "nvidia", "placement_id"), []byte(strconv.Itoa(placement)), 0644))
}

func newPackingSysfs(t *testing.T, query string) (testVendorVFIOSysfs, *int) {
	t.Helper()
	supported, err := os.ReadFile(filepath.Join("testdata", "nvidia-smi-vgpu-supported-l40s.txt"))
	require.NoError(t, err)
	sysfs := newTestVendorVFIOSysfs(t)
	calls := 0
	sysfs.nvidiaSMIFunc = func(_ context.Context, args ...string) ([]byte, error) {
		calls++
		if args[0] == "-q" {
			return []byte(query), nil
		}
		return supported, nil
	}
	return sysfs, &calls
}

func TestVendorVFIOConfigurePacksPlacement(t *testing.T) {
	sysfs, calls := newPackingSysfs(t, testHeterogeneousQuery)
	// The driver put the new 2Q at the lowest free placement, 0, which
	// overlaps the 4Q placement at 0. Placement 6 overlaps none. The clean
	// sibling occupies nothing, and the 2Q at 6 is on another GPU.
	sysfs.addPlacedVF(t, "0000:82:00.4", "42", "0", 0)
	sysfs.addPlacedVF(t, "0000:82:00.5", "43", testL40S2Q, 4)
	sysfs.addVF(t, "0000:82:00.0", "0000:82:00.7", "45", "0", testCreatableTypes)
	sysfs.addVF(t, "0000:83:00.0", "0000:83:00.4", "46", testL40S2Q, testCreatableTypes)
	require.NoError(t, os.WriteFile(filepath.Join(sysfs.pciDevicesPath, "0000:83:00.4", "nvidia", "placement_id"), []byte("6"), 0644))

	require.NoError(t, sysfs.configure(context.Background(), "0000:82:00.4", testL40S2Q))
	assertFileValue(t, filepath.Join(sysfs.pciDevicesPath, "0000:82:00.4", "nvidia", "placement_id"), "6")

	sysfs.addPlacedVF(t, "0000:82:00.6", "44", "0", 0)
	require.NoError(t, sysfs.configure(context.Background(), "0000:82:00.6", testL40S2Q))
	assertFileValue(t, filepath.Join(sysfs.pciDevicesPath, "0000:82:00.6", "nvidia", "placement_id"), "16")
	assert.Equal(t, 2, *calls, "geometry is loaded once per GPU")
}

func TestVendorVFIOConfigureCachesFailedPlacementLoad(t *testing.T) {
	sysfs := newTestVendorVFIOSysfs(t)
	calls := 0
	sysfs.nvidiaSMIFunc = func(context.Context, ...string) ([]byte, error) {
		calls++
		return nil, errors.New("exit status 9")
	}
	sysfs.addPlacedVF(t, "0000:82:00.4", "42", "0", 0)
	sysfs.addPlacedVF(t, "0000:82:00.5", "43", "0", 0)

	require.NoError(t, sysfs.configure(context.Background(), "0000:82:00.4", testL40S2Q))
	require.NoError(t, sysfs.configure(context.Background(), "0000:82:00.5", testL40S2Q))
	assert.Equal(t, 1, calls, "a failed load is cached too")
}

func TestVendorVFIOConfigureKeepsDriverPlacement(t *testing.T) {
	tests := []struct {
		name   string
		setup  func(sysfs *testVendorVFIOSysfs)
		logged string
	}{
		{
			name:   "equal-size mode",
			logged: "equal-size vGPU mode",
			setup: func(sysfs *testVendorVFIOSysfs) {
				query := strings.Replace(testHeterogeneousQuery, "Enabled", "Disabled", 1)
				sysfs.nvidiaSMIFunc = func(context.Context, ...string) ([]byte, error) { return []byte(query), nil }
			},
		},
		{
			name:   "nvidia-smi fails",
			logged: "exit status 9",
			setup: func(sysfs *testVendorVFIOSysfs) {
				sysfs.nvidiaSMIFunc = func(context.Context, ...string) ([]byte, error) { return nil, errors.New("exit status 9") }
			},
		},
		{
			name:   "sibling of unknown type",
			logged: "no known placement size",
			setup: func(sysfs *testVendorVFIOSysfs) {
				sysfs.addPlacedVF(t, "0000:82:00.6", "44", "999", 8)
			},
		},
		{
			name:   "driver refuses placement",
			logged: "driver refused vGPU placement",
			setup: func(sysfs *testVendorVFIOSysfs) {
				sysfs.writeFileFunc = func(name string, data []byte, perm os.FileMode) error {
					if filepath.Base(name) == "placement_id" {
						return syscall.EIO
					}
					return os.WriteFile(name, data, perm)
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sysfs, _ := newPackingSysfs(t, testHeterogeneousQuery)
			sysfs.addPlacedVF(t, "0000:82:00.4", "42", "0", 0)
			sysfs.addPlacedVF(t, "0000:82:00.5", "43", testL40S2Q, 4)
			tt.setup(&sysfs)
			var logs bytes.Buffer
			ctx := logger.AddToContext(context.Background(), slog.New(slog.NewTextHandler(&logs, nil)))

			require.NoError(t, sysfs.configure(ctx, "0000:82:00.4", testL40S2Q))
			assertFileValue(t, filepath.Join(sysfs.pciDevicesPath, "0000:82:00.4", "nvidia", "current_vgpu_type"), testL40S2Q)
			assertFileValue(t, filepath.Join(sysfs.pciDevicesPath, "0000:82:00.4", "nvidia", "placement_id"), "0")
			assert.Contains(t, logs.String(), tt.logged)
		})
	}
}
