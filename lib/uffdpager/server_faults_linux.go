//go:build linux

package uffdpager

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

const (
	uffdEventPagefault = 0x12
	uffdEventRemove    = 0x15
	uffdEventUnmap     = 0x16
	uffdioCopy         = 0xc028aa03
	uffdMsgSize        = 32
)

type guestRegionUffdMapping struct {
	BaseHostVirtAddr uint64 `json:"base_host_virt_addr"`
	Size             uint64 `json:"size"`
	Offset           uint64 `json:"offset"`
	PageSize         uint64 `json:"page_size"`
	PageSizeKiB      uint64 `json:"page_size_kib,omitempty"`
}

type uffdioCopyArgs struct {
	dst  uint64
	src  uint64
	len  uint64
	mode uint64
	copy int64
}

type uffdEvent struct {
	kind byte
	addr int64
}

func (s *session) handleFaults(mappings []guestRegionUffdMapping) {
	fd := s.uffdFD
	_ = unix.SetNonblock(fd, true)
	pollFDs := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
	buf := make([]byte, uffdMsgSize)
	var deferred []uffdEvent
	for {
		if len(deferred) == 0 {
			n, err := unix.Poll(pollFDs, -1)
			if err != nil {
				if err == unix.EINTR {
					continue
				}
				return
			}
			if n == 0 || pollFDs[0].Revents&unix.POLLIN == 0 {
				if pollFDs[0].Revents&(unix.POLLHUP|unix.POLLERR|unix.POLLNVAL) != 0 {
					return
				}
				continue
			}
		}

		events := deferred
		deferred = nil
		for {
			event, ok, err := readUFFDEvent(fd, buf)
			if err != nil {
				log.Printf("uffd session %s read uffd event: %v", s.id, err)
				return
			}
			if !ok {
				break
			}
			events = append(events, event)
		}

		for _, event := range events {
			switch event.kind {
			case uffdEventPagefault:
				if err := s.servePageFault(mappings, event.addr); err != nil {
					if errors.Is(err, unix.EAGAIN) {
						deferred = append(deferred, event)
						continue
					}
					s.server.copyErrors.Add(1)
					log.Printf("uffd session %s page fault at %#x: %v", s.id, event.addr, err)
				}
			case uffdEventRemove, uffdEventUnmap:
				// Reading remove/unmap events clears the pending state that can
				// make UFFDIO_COPY return EAGAIN.
			default:
				log.Printf("uffd session %s ignoring unexpected uffd event %#x", s.id, event.kind)
			}
		}

		if len(deferred) > 0 {
			time.Sleep(time.Millisecond)
		}
	}
}

func readUFFDEvent(fd int, buf []byte) (uffdEvent, bool, error) {
	read, err := unix.Read(fd, buf)
	if err != nil {
		if err == unix.EINTR {
			return uffdEvent{}, false, nil
		}
		if err == unix.EAGAIN || err == unix.EWOULDBLOCK {
			return uffdEvent{}, false, nil
		}
		return uffdEvent{}, false, err
	}
	if read == 0 {
		return uffdEvent{}, false, io.EOF
	}
	if read < 32 {
		return uffdEvent{}, false, fmt.Errorf("short uffd event read: %d bytes", read)
	}
	event := uffdEvent{kind: buf[0]}
	if event.kind == uffdEventPagefault {
		event.addr = int64(nativeEndian.Uint64(buf[16:24]))
	}
	return event, true, nil
}

func (s *session) servePageFault(mappings []guestRegionUffdMapping, faultAddr int64) error {
	start := time.Now()
	active := s.server.activeFaults.Add(1)
	atomicMaxInt64(&s.server.maxConcurrentFaults, active)
	defer func() {
		s.server.activeFaults.Add(-1)
		nanos := time.Since(start).Nanoseconds()
		s.server.faultNanos.Add(nanos)
		atomicMaxInt64(&s.server.faultMaxNanos, nanos)
	}()

	mapping, pageAddr, pageOffset, pageSize, ok := findMapping(mappings, faultAddr)
	if !ok {
		return fmt.Errorf("fault address %#x outside guest mappings", faultAddr)
	}
	_ = mapping

	s.server.faults.Add(1)
	page, err := s.readPage(pageOffset, pageSize)
	if err != nil {
		return err
	}
	copyStart := time.Now()
	if err := uffdCopy(s.uffdFD, uint64(pageAddr), page); err != nil {
		nanos := time.Since(copyStart).Nanoseconds()
		s.server.copyNanos.Add(nanos)
		atomicMaxInt64(&s.server.copyMaxNanos, nanos)
		return err
	}
	nanos := time.Since(copyStart).Nanoseconds()
	s.server.copyNanos.Add(nanos)
	atomicMaxInt64(&s.server.copyMaxNanos, nanos)
	s.server.copies.Add(1)
	return nil
}

func (s *session) readPage(offset int64, size int) ([]byte, error) {
	start := time.Now()
	defer func() {
		nanos := time.Since(start).Nanoseconds()
		s.server.readPageNanos.Add(nanos)
		atomicMaxInt64(&s.server.readPageMaxNanos, nanos)
	}()

	if page, ok := s.server.cache.Borrow(s.cacheKey, offset, size); ok {
		return page, nil
	}

	page := make([]byte, size)
	readStart := time.Now()
	n, err := s.backingFile.ReadAt(page, offset)
	readNanos := time.Since(readStart).Nanoseconds()
	s.server.backingReadNanos.Add(readNanos)
	atomicMaxInt64(&s.server.backingReadMaxNanos, readNanos)
	if err != nil {
		return nil, fmt.Errorf("read backing page at %d: read %d/%d bytes: %w", offset, n, size, err)
	}
	if n != size {
		return nil, fmt.Errorf("short backing page read at %d: read %d/%d bytes", offset, n, size)
	}
	s.server.backingBytesRead.Add(int64(n))
	s.server.cache.Add(s.cacheKey, offset, page)
	return page, nil
}

func recvMappingsAndFD(conn *net.UnixConn) ([]guestRegionUffdMapping, int, error) {
	buf := make([]byte, 128<<10)
	oob := make([]byte, unix.CmsgSpace(4))
	n, oobn, _, _, err := conn.ReadMsgUnix(buf, oob)
	if err != nil {
		return nil, -1, err
	}
	if n == 0 {
		return nil, -1, fmt.Errorf("empty mapping payload")
	}

	msgs, err := unix.ParseSocketControlMessage(oob[:oobn])
	if err != nil {
		return nil, -1, err
	}
	var fds []int
	for _, msg := range msgs {
		rights, err := unix.ParseUnixRights(&msg)
		if err != nil {
			continue
		}
		fds = append(fds, rights...)
	}
	if len(fds) == 0 {
		return nil, -1, fmt.Errorf("no uffd file descriptor received")
	}
	for _, extra := range fds[1:] {
		_ = unix.Close(extra)
	}

	mappings, err := decodeMappings(buf[:n])
	if err != nil {
		_ = unix.Close(fds[0])
		return nil, -1, err
	}
	return mappings, fds[0], nil
}

func decodeMappings(data []byte) ([]guestRegionUffdMapping, error) {
	var mappings []guestRegionUffdMapping
	if err := json.Unmarshal(data, &mappings); err == nil {
		return normalizeMappings(mappings)
	}

	var wrapped struct {
		Mappings []guestRegionUffdMapping `json:"mappings"`
	}
	if err := json.Unmarshal(data, &wrapped); err != nil {
		return nil, fmt.Errorf("decode uffd mappings: %w", err)
	}
	return normalizeMappings(wrapped.Mappings)
}

func normalizeMappings(mappings []guestRegionUffdMapping) ([]guestRegionUffdMapping, error) {
	if len(mappings) == 0 {
		return nil, fmt.Errorf("no uffd mappings received")
	}
	for i := range mappings {
		if mappings[i].PageSize == 0 {
			mappings[i].PageSize = mappings[i].PageSizeKiB
		}
		if mappings[i].PageSize == 0 {
			mappings[i].PageSize = uint64(os.Getpagesize())
		}
		if mappings[i].Size == 0 {
			return nil, fmt.Errorf("mapping %d has zero size", i)
		}
		if mappings[i].PageSize&(mappings[i].PageSize-1) != 0 {
			return nil, fmt.Errorf("mapping %d page size %d is not a power of two", i, mappings[i].PageSize)
		}
	}
	return mappings, nil
}

func findMapping(mappings []guestRegionUffdMapping, faultAddr int64) (guestRegionUffdMapping, int64, int64, int, bool) {
	for _, mapping := range mappings {
		start := int64(mapping.BaseHostVirtAddr)
		end := start + int64(mapping.Size)
		if faultAddr < start || faultAddr >= end {
			continue
		}
		pageSize := int64(mapping.PageSize)
		pageAddr := faultAddr &^ (pageSize - 1)
		pageOffset := int64(mapping.Offset) + (pageAddr - start)
		return mapping, pageAddr, pageOffset, int(pageSize), true
	}
	return guestRegionUffdMapping{}, 0, 0, 0, false
}

func uffdCopy(fd int, dst uint64, page []byte) error {
	if len(page) == 0 {
		return fmt.Errorf("empty page")
	}
	args := uffdioCopyArgs{
		dst: dst,
		src: uint64(uintptr(unsafe.Pointer(&page[0]))),
		len: uint64(len(page)),
	}
	_, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), uintptr(uffdioCopy), uintptr(unsafe.Pointer(&args)))
	if errno == 0 || errno == syscall.EEXIST {
		return nil
	}
	return errno
}

func atomicMaxInt64(target *atomic.Int64, candidate int64) {
	for {
		current := target.Load()
		if candidate <= current || target.CompareAndSwap(current, candidate) {
			return
		}
	}
}

var nativeEndian = nativeByteOrder()

type byteOrder interface {
	Uint64([]byte) uint64
}

func nativeByteOrder() byteOrder {
	var x uint16 = 0x1
	b := (*[2]byte)(unsafe.Pointer(&x))
	if b[0] == 0x1 {
		return littleEndian{}
	}
	return bigEndian{}
}

type littleEndian struct{}

func (littleEndian) Uint64(b []byte) uint64 {
	return uint64(b[0]) | uint64(b[1])<<8 | uint64(b[2])<<16 | uint64(b[3])<<24 |
		uint64(b[4])<<32 | uint64(b[5])<<40 | uint64(b[6])<<48 | uint64(b[7])<<56
}

type bigEndian struct{}

func (bigEndian) Uint64(b []byte) uint64 {
	return uint64(b[7]) | uint64(b[6])<<8 | uint64(b[5])<<16 | uint64(b[4])<<24 |
		uint64(b[3])<<32 | uint64(b[2])<<40 | uint64(b[1])<<48 | uint64(b[0])<<56
}
