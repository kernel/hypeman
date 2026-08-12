//go:build linux

package cloudhypervisor

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

func mergeCloudHypervisorDiff(snapshotDir string) (diffMergeStats, error) {
	var stats diffMergeStats
	basePath := filepath.Join(snapshotDir, cloudHypervisorMemoryFile)
	diffPath := filepath.Join(snapshotDir, cloudHypervisorDiffMemoryFile)

	src, err := os.Open(diffPath)
	if err != nil {
		return stats, fmt.Errorf("open diff snapshot memory: %w", err)
	}
	defer src.Close()
	// Resolve delayed allocation before cloning extents. FICLONERANGE can
	// otherwise clone the pre-write extent state while dirty pages still live
	// only in the source file's page cache.
	if err := src.Sync(); err != nil {
		return stats, fmt.Errorf("sync diff snapshot memory: %w", err)
	}
	dst, err := os.OpenFile(basePath, os.O_RDWR, 0)
	if err != nil {
		return stats, fmt.Errorf("open retained snapshot memory: %w", err)
	}
	defer dst.Close()

	srcInfo, err := src.Stat()
	if err != nil {
		return stats, fmt.Errorf("stat diff snapshot memory: %w", err)
	}
	dstInfo, err := dst.Stat()
	if err != nil {
		return stats, fmt.Errorf("stat retained snapshot memory: %w", err)
	}
	if srcInfo.Size() != dstInfo.Size() {
		return stats, fmt.Errorf("diff snapshot memory size %d does not match baseline %d", srcInfo.Size(), dstInfo.Size())
	}

	srcFD := int(src.Fd())
	dstFD := int(dst.Fd())
	size := srcInfo.Size()
	offset := int64(0)
	cloneRanges := !ExperimentalHotplugOverlayEnabled()
	buf := make([]byte, 1<<20)

	for offset < size {
		dataStart, err := unix.Seek(srcFD, offset, unix.SEEK_DATA)
		if err != nil {
			if errors.Is(err, unix.ENXIO) {
				break
			}
			return stats, fmt.Errorf("seek diff data at %d: %w", offset, err)
		}
		dataEnd, err := unix.Seek(srcFD, dataStart, unix.SEEK_HOLE)
		if err != nil {
			if errors.Is(err, unix.ENXIO) {
				dataEnd = size
			} else {
				return stats, fmt.Errorf("seek diff hole at %d: %w", dataStart, err)
			}
		}
		if dataEnd > size {
			dataEnd = size
		}
		if dataEnd <= dataStart {
			return stats, fmt.Errorf("invalid diff extent [%d,%d)", dataStart, dataEnd)
		}

		length := dataEnd - dataStart
		stats.DeltaBytes += length
		stats.ExtentCount++
		if cloneRanges {
			clone := unix.FileCloneRange{
				Src_fd:      int64(srcFD),
				Src_offset:  uint64(dataStart),
				Src_length:  uint64(length),
				Dest_offset: uint64(dataStart),
			}
			if err := unix.IoctlFileCloneRange(dstFD, &clone); err == nil {
				stats.ReflinkedBytes += length
				offset = dataEnd
				continue
			} else if !diffRangeCloneUnsupported(err) {
				return stats, fmt.Errorf("reflink diff extent [%d,%d): %w", dataStart, dataEnd, err)
			}
			cloneRanges = false
		}

		if err := copyDiffExtent(srcFD, dstFD, dataStart, length, buf); err != nil {
			return stats, fmt.Errorf("copy diff extent [%d,%d): %w", dataStart, dataEnd, err)
		}
		stats.CopiedBytes += length
		offset = dataEnd
	}

	if err := dst.Sync(); err != nil {
		return stats, fmt.Errorf("sync merged snapshot memory: %w", err)
	}
	if err := os.Remove(diffPath); err != nil {
		return stats, fmt.Errorf("remove merged diff snapshot: %w", err)
	}
	dir, err := os.Open(snapshotDir)
	if err != nil {
		return stats, fmt.Errorf("open snapshot directory: %w", err)
	}
	if err := dir.Sync(); err != nil {
		_ = dir.Close()
		return stats, fmt.Errorf("sync snapshot directory: %w", err)
	}
	if err := dir.Close(); err != nil {
		return stats, fmt.Errorf("close snapshot directory: %w", err)
	}
	return stats, nil
}

func copyDiffExtent(srcFD, dstFD int, offset, length int64, buf []byte) error {
	position := offset
	remaining := length
	for remaining > 0 {
		chunk := int64(len(buf))
		if remaining < chunk {
			chunk = remaining
		}
		n, err := unix.Pread(srcFD, buf[:int(chunk)], position)
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrUnexpectedEOF
		}
		written := 0
		for written < n {
			wn, err := unix.Pwrite(dstFD, buf[written:n], position+int64(written))
			if err != nil {
				return err
			}
			if wn == 0 {
				return io.ErrShortWrite
			}
			written += wn
		}
		position += int64(n)
		remaining -= int64(n)
	}
	return nil
}

func diffRangeCloneUnsupported(err error) bool {
	return errors.Is(err, unix.EINVAL) ||
		errors.Is(err, unix.ENOTSUP) ||
		errors.Is(err, unix.EOPNOTSUPP) ||
		errors.Is(err, unix.EXDEV) ||
		errors.Is(err, unix.ENOTTY) ||
		errors.Is(err, unix.ETXTBSY)
}
