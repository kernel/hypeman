package images

import (
	"context"
	"encoding/binary"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

const (
	erofsSuperOffset                 = 1024
	erofsBlockBitsOffset             = erofsSuperOffset + 12
	erofsBlocksOffset                = erofsSuperOffset + 36
	erofsExtraDevicesOffset          = erofsSuperOffset + 86
	erofsDeviceTableOffset           = erofsSuperOffset + 88
	erofsDeviceSlotSize              = 128
	erofsDeviceMappedBlockAddressOff = 68
	erofsMagic                       = 0xE0F5E1E2
)

// buildFsmergeMetadata creates the metadata device for a set of materialized
// EROFS layers. The layer bytes remain in their content-addressed artifacts;
// this output contains the merged inode metadata and references to external
// device slots.
func buildFsmergeMetadata(ctx context.Context, outputPath string, layers []string) (int64, error) {
	if len(layers) == 0 {
		return 0, fmt.Errorf("fsmerge requires at least one layer")
	}
	if err := os.MkdirAll(filepath.Dir(outputPath), 0755); err != nil {
		return 0, fmt.Errorf("create fsmerge metadata directory: %w", err)
	}
	args := []string{
		"-zlz4",
		"-E", "^inline_data",
		"--aufs",
		"--ovlfs-strip=1",
		outputPath,
	}
	args = append(args, layers...)
	cmd := exec.CommandContext(ctx, "mkfs.erofs", args...)
	if output, err := cmd.CombinedOutput(); err != nil {
		return 0, fmt.Errorf("mkfs.erofs fsmerge metadata: %w, output: %s", err, output)
	}
	if err := patchFsmergeDeviceMappings(outputPath, layers); err != nil {
		return 0, fmt.Errorf("patch fsmerge device mappings: %w", err)
	}
	info, err := os.Stat(outputPath)
	if err != nil {
		return 0, fmt.Errorf("stat fsmerge metadata: %w", err)
	}
	return info.Size(), nil
}

// patchFsmergeDeviceMappings fills the external device offsets that
// erofs-utils currently leaves at zero in rebuild mode. Offsets are measured
// in filesystem blocks from the beginning of the eventual concatenated device.
func patchFsmergeDeviceMappings(path string, layers []string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if len(data) < erofsSuperOffset+128 {
		return fmt.Errorf("metadata image is smaller than an EROFS superblock")
	}
	super := data[erofsSuperOffset:]
	if binary.LittleEndian.Uint32(super[0:4]) != erofsMagic {
		return fmt.Errorf("invalid EROFS magic")
	}
	blockBits := super[erofsBlockBitsOffset-erofsSuperOffset]
	if blockBits >= 63 {
		return fmt.Errorf("invalid EROFS block size bits: %d", blockBits)
	}
	blockSize := int64(1) << blockBits
	if blockSize != 4096 {
		return fmt.Errorf("unsupported EROFS block size: %d", blockSize)
	}
	extraDevices := int(binary.LittleEndian.Uint16(super[erofsExtraDevicesOffset-erofsSuperOffset:]))
	if extraDevices != len(layers) {
		return fmt.Errorf("metadata has %d device slots, want %d", extraDevices, len(layers))
	}
	slotOffset := int64(binary.LittleEndian.Uint16(super[erofsDeviceTableOffset-erofsSuperOffset:])) * 128
	if slotOffset < 0 || slotOffset+int64(len(layers))*erofsDeviceSlotSize+erofsDeviceMappedBlockAddressOff+4 > int64(len(super)) {
		return fmt.Errorf("device table is outside the superblock")
	}
	primaryBlocks := int64(binary.LittleEndian.Uint32(super[erofsBlocksOffset-erofsSuperOffset:]))
	if primaryBlocks == 0 {
		return fmt.Errorf("metadata primary device has no blocks")
	}

	mappedBlock := primaryBlocks
	for i, layer := range layers {
		info, err := os.Stat(layer)
		if err != nil {
			return fmt.Errorf("stat layer %s: %w", layer, err)
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("layer %s is not a regular file", layer)
		}
		if info.Size()%blockSize != 0 {
			return fmt.Errorf("layer %s is not block aligned", layer)
		}
		slot := super[slotOffset+int64(i)*erofsDeviceSlotSize:]
		binary.LittleEndian.PutUint32(slot[erofsDeviceMappedBlockAddressOff:], uint32(mappedBlock))
		mappedBlock += info.Size() / blockSize
	}

	binary.LittleEndian.PutUint32(super[4:8], 0)
	checksum := erofsCRC32C(super)
	binary.LittleEndian.PutUint32(super[4:8], checksum)
	return os.WriteFile(path, data, 0644)
}

func erofsCRC32C(data []byte) uint32 {
	crc := ^uint32(0)
	for _, b := range data {
		crc ^= uint32(b)
		for i := 0; i < 8; i++ {
			if crc&1 != 0 {
				crc = (crc >> 1) ^ 0x82f63b78
			} else {
				crc >>= 1
			}
		}
	}
	return crc
}
