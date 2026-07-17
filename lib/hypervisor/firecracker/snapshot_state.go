package firecracker

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
)

const firecrackerSnapshotCRC64JonesReversedPoly uint64 = 0x95ac9329ac4bc9b5

type snapshotStatePathRewrite struct {
	Source string
	Target string
}

func rewriteSnapshotStatePathsForFork(statePath string, rewrites []snapshotStatePathRewrite) (bool, error) {
	if len(rewrites) == 0 {
		return false, nil
	}

	data, err := os.ReadFile(statePath)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, fmt.Errorf("read firecracker snapshot state: %w", err)
	}
	if len(data) < 8 {
		return false, fmt.Errorf("firecracker snapshot state is too short")
	}
	if firecrackerSnapshotCRC64(data) != 0 {
		return false, fmt.Errorf("firecracker snapshot state CRC64 validation failed before path rewrite")
	}

	body := append([]byte(nil), data[:len(data)-8]...)
	replaced := 0
	targetsBySource := make(map[string][]string)
	sources := make([]string, 0, len(rewrites))
	seen := make(map[string]struct{}, len(rewrites))
	for _, rewrite := range rewrites {
		source := filepath.Clean(rewrite.Source)
		target := filepath.Clean(rewrite.Target)
		if source == "." || target == "." || source == target {
			continue
		}
		key := source + "\x00" + target
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		if _, ok := targetsBySource[source]; !ok {
			sources = append(sources, source)
		}
		targetsBySource[source] = append(targetsBySource[source], target)
	}

	for _, source := range sources {
		sourceBytes := []byte(source)
		count := bytes.Count(body, sourceBytes)
		if count == 0 {
			continue
		}
		var targetBytes []byte
		for _, target := range targetsBySource[source] {
			candidate := []byte(target)
			if len(sourceBytes) == len(candidate) {
				targetBytes = candidate
				break
			}
		}
		if len(targetBytes) == 0 {
			return false, nil
		}
		body = bytes.ReplaceAll(body, sourceBytes, targetBytes)
		replaced += count
	}
	if replaced == 0 {
		return false, nil
	}

	out := make([]byte, len(body)+8)
	copy(out, body)
	binary.LittleEndian.PutUint64(out[len(out)-8:], firecrackerSnapshotCRC64(body))
	if firecrackerSnapshotCRC64(out) != 0 {
		return false, fmt.Errorf("firecracker snapshot state CRC64 validation failed after path rewrite")
	}
	if err := os.WriteFile(statePath, out, 0644); err != nil {
		return false, fmt.Errorf("write firecracker snapshot state: %w", err)
	}
	return true, nil
}

func firecrackerSnapshotCRC64(data []byte) uint64 {
	crc := uint64(0)
	for _, b := range data {
		crc ^= uint64(b)
		for i := 0; i < 8; i++ {
			if crc&1 != 0 {
				crc = (crc >> 1) ^ firecrackerSnapshotCRC64JonesReversedPoly
			} else {
				crc >>= 1
			}
		}
	}
	return crc
}
