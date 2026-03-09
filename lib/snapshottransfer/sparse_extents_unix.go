//go:build darwin || linux

package snapshottransfer

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

type fileDataExtent struct {
	FileOffset int64
	Length     int64
}

func listFileDataExtents(path string) ([]fileDataExtent, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	size := info.Size()
	if size == 0 {
		return nil, nil
	}

	fd := int(f.Fd())
	offset := int64(0)
	extents := make([]fileDataExtent, 0, 4)
	for offset < size {
		dataStart, err := unix.Seek(fd, offset, unix.SEEK_DATA)
		if err != nil {
			if errors.Is(err, unix.ENXIO) {
				break
			}
			if errors.Is(err, unix.ENOTSUP) || errors.Is(err, unix.EOPNOTSUPP) || errors.Is(err, unix.EINVAL) {
				return []fileDataExtent{{FileOffset: 0, Length: size}}, nil
			}
			return nil, fmt.Errorf("seek data: %w", err)
		}
		if dataStart >= size {
			break
		}

		holeStart, err := unix.Seek(fd, dataStart, unix.SEEK_HOLE)
		if err != nil {
			if errors.Is(err, unix.ENXIO) {
				holeStart = size
			} else {
				return nil, fmt.Errorf("seek hole: %w", err)
			}
		}
		if holeStart > size {
			holeStart = size
		}
		if holeStart > dataStart {
			extents = append(extents, fileDataExtent{FileOffset: dataStart, Length: holeStart - dataStart})
		}
		offset = holeStart
	}
	return extents, nil
}
