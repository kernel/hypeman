package images

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPatchFsmergeDeviceMappings(t *testing.T) {
	dir := t.TempDir()
	metadata := filepath.Join(dir, "fsmeta.erofs")
	layers := []string{filepath.Join(dir, "layer0.erofs"), filepath.Join(dir, "layer1.erofs")}
	for _, path := range layers {
		require.NoError(t, os.WriteFile(path, make([]byte, 4096*3), 0644))
	}

	data := make([]byte, 8192)
	super := data[erofsSuperOffset:]
	binary.LittleEndian.PutUint32(super[0:4], erofsMagic)
	super[erofsBlockBitsOffset-erofsSuperOffset] = 12
	binary.LittleEndian.PutUint32(super[erofsBlocksOffset-erofsSuperOffset:], 1)
	binary.LittleEndian.PutUint16(super[erofsExtraDevicesOffset-erofsSuperOffset:], uint16(len(layers)))
	deviceTableOffset := erofsSuperOffset + 128
	binary.LittleEndian.PutUint16(super[erofsDeviceTableOffset-erofsSuperOffset:], uint16(deviceTableOffset/erofsDeviceSlotSize))
	for i := range layers {
		slot := data[deviceTableOffset+i*erofsDeviceSlotSize:]
		binary.LittleEndian.PutUint32(slot[erofsDeviceMappedBlockAddressOff:], 0)
	}
	require.NoError(t, os.WriteFile(metadata, data, 0644))

	require.NoError(t, patchFsmergeDeviceMappings(metadata, layers))
	patched, err := os.ReadFile(metadata)
	require.NoError(t, err)
	super = patched[erofsSuperOffset:]
	for i, want := range []uint32{1, 4} {
		slot := patched[deviceTableOffset+i*erofsDeviceSlotSize:]
		require.Equal(t, want, binary.LittleEndian.Uint32(slot[erofsDeviceMappedBlockAddressOff:]))
	}
	checksum := binary.LittleEndian.Uint32(super[4:8])
	binary.LittleEndian.PutUint32(super[4:8], 0)
	require.Equal(t, checksum, erofsCRC32C(super[:4096-erofsSuperOffset]))
}

func TestPatchFsmergeDeviceMappingsRejectsUnalignedLayer(t *testing.T) {
	dir := t.TempDir()
	metadata := filepath.Join(dir, "fsmeta.erofs")
	layer := filepath.Join(dir, "layer.erofs")
	data := make([]byte, 4096)
	super := data[erofsSuperOffset:]
	binary.LittleEndian.PutUint32(super[0:4], erofsMagic)
	super[erofsBlockBitsOffset-erofsSuperOffset] = 12
	binary.LittleEndian.PutUint32(super[erofsBlocksOffset-erofsSuperOffset:], 1)
	binary.LittleEndian.PutUint16(super[erofsExtraDevicesOffset-erofsSuperOffset:], 1)
	binary.LittleEndian.PutUint16(super[erofsDeviceTableOffset-erofsSuperOffset:], 1)
	require.NoError(t, os.WriteFile(metadata, data, 0644))
	require.NoError(t, os.WriteFile(layer, []byte("not aligned"), 0644))

	err := patchFsmergeDeviceMappings(metadata, []string{layer})
	require.ErrorContains(t, err, "not block aligned")
}
