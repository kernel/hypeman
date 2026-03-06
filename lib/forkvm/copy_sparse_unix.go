//go:build darwin || linux

package forkvm

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"

	"golang.org/x/sys/unix"
)

var (
	seekDataFn = func(fd int, offset int64) (int64, error) {
		return unix.Seek(fd, offset, unix.SEEK_DATA)
	}
	seekHoleFn = func(fd int, offset int64) (int64, error) {
		return unix.Seek(fd, offset, unix.SEEK_HOLE)
	}
)

func copyRegularFileSparse(srcPath, dstPath string, perms fs.FileMode) (retErr error) {
	src, err := os.Open(srcPath)
	if err != nil {
		return err
	}
	defer src.Close()

	info, err := src.Stat()
	if err != nil {
		return fmt.Errorf("stat source file: %w", err)
	}

	dst, err := os.OpenFile(dstPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, perms)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := dst.Close(); retErr == nil && cerr != nil {
			retErr = cerr
		}
	}()

	size := info.Size()
	if err := unix.Ftruncate(int(dst.Fd()), size); err != nil {
		return fmt.Errorf("truncate destination file: %w", err)
	}
	if size == 0 {
		return nil
	}

	srcFD := int(src.Fd())
	dstFD := int(dst.Fd())
	offset := int64(0)

	for offset < size {
		dataStart, err := seekDataFn(srcFD, offset)
		if err != nil {
			if errors.Is(err, unix.ENXIO) {
				break
			}
			if isSparseUnsupportedError(err) {
				return fmt.Errorf("%w: SEEK_DATA unsupported for %s: %v", ErrSparseCopyUnsupported, srcPath, err)
			}
			return fmt.Errorf("seek data at offset %d: %w", offset, err)
		}
		if dataStart >= size {
			break
		}

		dataEnd, err := seekHoleFn(srcFD, dataStart)
		if err != nil {
			if errors.Is(err, unix.ENXIO) {
				dataEnd = size
			} else if isSparseUnsupportedError(err) {
				return fmt.Errorf("%w: SEEK_HOLE unsupported for %s: %v", ErrSparseCopyUnsupported, srcPath, err)
			} else {
				return fmt.Errorf("seek hole at offset %d: %w", dataStart, err)
			}
		}

		if dataEnd > size {
			dataEnd = size
		}
		if dataEnd < dataStart {
			return fmt.Errorf("invalid sparse extent (%d..%d) for %s", dataStart, dataEnd, srcPath)
		}

		length := dataEnd - dataStart
		if length > 0 {
			if err := copyFileExtent(srcFD, dstFD, dataStart, length); err != nil {
				return fmt.Errorf("copy sparse extent [%d,%d): %w", dataStart, dataEnd, err)
			}
		}
		offset = dataEnd
	}

	return nil
}

func copyFileExtent(srcFD, dstFD int, offset, length int64) error {
	const chunkSize = 1 << 20 // 1 MiB
	buf := make([]byte, chunkSize)

	pos := offset
	remaining := length
	for remaining > 0 {
		toRead := int64(len(buf))
		if remaining < toRead {
			toRead = remaining
		}

		n, err := unix.Pread(srcFD, buf[:int(toRead)], pos)
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrUnexpectedEOF
		}

		written := 0
		for written < n {
			wn, werr := unix.Pwrite(dstFD, buf[written:n], pos+int64(written))
			if werr != nil {
				return werr
			}
			if wn == 0 {
				return io.ErrShortWrite
			}
			written += wn
		}

		pos += int64(n)
		remaining -= int64(n)
	}

	return nil
}

func isSparseUnsupportedError(err error) bool {
	return errors.Is(err, unix.EINVAL) || errors.Is(err, unix.ENOTSUP) || errors.Is(err, unix.EOPNOTSUPP)
}
