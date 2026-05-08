//go:build linux

package uffd

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

// userfaultfd ioctl numbers and feature flags. The constants are derived
// from <linux/userfaultfd.h>: _IOWR(0xAA, ...) with the size of each
// argument struct in bits 16–29.
const (
	uffdAPI        = 0xAA
	uffdAPIFeature = 0x0 // we only need missing-page faults; no extra features.

	uffdioAPI        = 0xC018AA3F // _IOWR(0xAA, 0x3F, struct uffdio_api{24})
	uffdioRegister   = 0xC020AA00 // _IOWR(0xAA, 0x00, struct uffdio_register{32})
	uffdioCopyIoctl  = 0xC028AA03 // _IOWR(0xAA, 0x03, struct uffdio_copy{40})
	uffdioZeropage   = 0xC020AA04 // _IOWR(0xAA, 0x04, struct uffdio_zeropage{32})
	uffdRegMissing   = 1 << 0
	uffdEventPagefnt = 0x12 // UFFD_EVENT_PAGEFAULT
)

// uffdMsg mirrors struct uffd_msg from <linux/userfaultfd.h>. It is a
// 32-byte fixed-size record; we only consume the pagefault arm.
type uffdMsg struct {
	Event     uint8
	_         uint8
	_         uint16
	_         uint32
	Pagefault struct {
		Flags   uint64
		Address uint64
		Ptid    uint32
		_       uint32
	}
}

// uffdioAPIArg is struct uffdio_api.
type uffdioAPIArg struct {
	API      uint64
	Features uint64
	Ioctls   uint64
}

// uffdioRegisterArg is struct uffdio_register.
type uffdioRegisterArg struct {
	Start  uint64
	Len    uint64
	Mode   uint64
	Ioctls uint64
}

// uffdioCopyArg is struct uffdio_copy.
type uffdioCopyArg struct {
	Dst  uint64
	Src  uint64
	Len  uint64
	Mode uint64
	Copy int64
}

// startListener opens the per-fork UDS, accepts firecracker's connection,
// receives the userfaultfd via SCM_RIGHTS plus the JSON handshake, and
// then runs the page-fault loop. The returned closer stops accept,
// signals the handler, and removes the socket file.
func (s *Server) startListener(ctx context.Context, forkID string, socketPath string) (func() error, error) {
	// Remove any stale socket file from a prior run; UDS bind fails otherwise.
	_ = os.Remove(socketPath)
	ln, err := net.Listen("unix", socketPath)
	if err != nil {
		return nil, fmt.Errorf("uffd: listen %s: %w", socketPath, err)
	}

	hctx, hcancel := context.WithCancel(ctx)

	var (
		wg     sync.WaitGroup
		mu     sync.Mutex
		uffdFd int = -1
		closed bool
	)

	closer := func() error {
		mu.Lock()
		if closed {
			mu.Unlock()
			wg.Wait()
			return nil
		}
		closed = true
		fd := uffdFd
		uffdFd = -1
		mu.Unlock()

		hcancel()
		_ = ln.Close()
		if fd >= 0 {
			_ = unix.Close(fd)
		}
		wg.Wait()
		_ = os.Remove(socketPath)
		return nil
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()

		fd, regions, err := receiveHandshake(conn)
		if err != nil {
			return
		}
		mu.Lock()
		if closed {
			mu.Unlock()
			_ = unix.Close(fd)
			return
		}
		uffdFd = fd
		mu.Unlock()

		if err := uffdAPIHandshake(fd); err != nil {
			return
		}
		for _, r := range regions {
			if err := uffdRegisterRegion(fd, r); err != nil {
				return
			}
		}

		s.installPrefetcher(forkID, func(list *HotPageList) error {
			return s.prefetchInto(fd, regions, list)
		})

		s.servePageFaults(hctx, fd, regions, forkID)
	}()

	return closer, nil
}

// receiveHandshake reads firecracker's JSON payload and the userfaultfd
// over a single recvmsg(2) call. Firecracker sends them together; if the
// kernel splits them across reads we loop until the fd arrives.
func receiveHandshake(conn net.Conn) (int, []MemoryRegion, error) {
	uc, ok := conn.(*net.UnixConn)
	if !ok {
		return -1, nil, errors.New("uffd: connection is not a unix socket")
	}
	f, err := uc.File()
	if err != nil {
		return -1, nil, fmt.Errorf("uffd: get fd from unix conn: %w", err)
	}
	defer f.Close()

	// Read until we have the SCM_RIGHTS fd. The JSON body is small, so
	// a 4 KiB buffer plus one OOB control message is plenty.
	buf := make([]byte, 4096)
	oob := make([]byte, unix.CmsgSpace(4))
	var (
		jsonBytes []byte
		fd        int = -1
	)
	for fd < 0 {
		n, oobn, _, _, err := unix.Recvmsg(int(f.Fd()), buf, oob, 0)
		if err != nil {
			return -1, nil, fmt.Errorf("uffd: recvmsg: %w", err)
		}
		if n > 0 {
			jsonBytes = append(jsonBytes, buf[:n]...)
		}
		if oobn > 0 {
			scms, perr := unix.ParseSocketControlMessage(oob[:oobn])
			if perr != nil {
				return -1, nil, fmt.Errorf("uffd: parse cmsg: %w", perr)
			}
			for _, scm := range scms {
				fds, ferr := unix.ParseUnixRights(&scm)
				if ferr != nil {
					return -1, nil, fmt.Errorf("uffd: parse fds: %w", ferr)
				}
				if len(fds) > 0 {
					fd = fds[0]
					for _, extra := range fds[1:] {
						_ = unix.Close(extra)
					}
				}
			}
		}
		if n == 0 && oobn == 0 {
			return -1, nil, io.ErrUnexpectedEOF
		}
	}

	hs, err := parseHandshake(jsonBytes)
	if err != nil {
		_ = unix.Close(fd)
		return -1, nil, err
	}
	return fd, hs.Mappings, nil
}

func uffdAPIHandshake(fd int) error {
	api := uffdioAPIArg{API: uffdAPI, Features: uffdAPIFeature}
	if err := ioctl(fd, uffdioAPI, unsafe.Pointer(&api)); err != nil {
		return fmt.Errorf("uffd: UFFDIO_API: %w", err)
	}
	return nil
}

func uffdRegisterRegion(fd int, r MemoryRegion) error {
	reg := uffdioRegisterArg{
		Start: uint64(r.BaseHostAddr),
		Len:   r.Size,
		Mode:  uffdRegMissing,
	}
	if err := ioctl(fd, uffdioRegister, unsafe.Pointer(&reg)); err != nil {
		return fmt.Errorf("uffd: UFFDIO_REGISTER: %w", err)
	}
	return nil
}

// servePageFaults blocks reading uffd events on fd. For each
// UFFD_EVENT_PAGEFAULT we look up the region containing the faulting
// address, read a page from the template mem-file, and call UFFDIO_COPY
// to satisfy the fault.
func (s *Server) servePageFaults(ctx context.Context, fd int, regions []MemoryRegion, forkID string) {
	page := make([]byte, s.pageSize)
	var msg uffdMsg
	msgSize := int(unsafe.Sizeof(msg))
	rawBuf := make([]byte, msgSize)

	for {
		if ctx.Err() != nil {
			return
		}
		n, err := unix.Read(fd, rawBuf)
		if err != nil {
			if errors.Is(err, syscall.EINTR) {
				continue
			}
			return
		}
		if n != msgSize {
			return
		}
		event := rawBuf[0]
		if event != uffdEventPagefnt {
			continue
		}
		// pagefault.address starts at offset 16 of uffd_msg.
		addr := binary.LittleEndian.Uint64(rawBuf[16:24])
		if err := s.copyPageForFault(fd, regions, addr, page); err != nil {
			return
		}
	}
}

func (s *Server) copyPageForFault(fd int, regions []MemoryRegion, addr uint64, page []byte) error {
	pageSize := uint64(s.pageSize)
	pageStart := addr &^ (pageSize - 1)

	for idx, r := range regions {
		base := uint64(r.BaseHostAddr)
		if pageStart < base || pageStart >= base+r.Size {
			continue
		}
		regionOff := pageStart - base
		offset := int64(r.MemFileOffset + regionOff)
		if _, err := s.memFile.ReadAt(page, offset); err != nil && !errors.Is(err, io.EOF) {
			return fmt.Errorf("uffd: read template at %d: %w", offset, err)
		}
		copyArg := uffdioCopyArg{
			Dst: pageStart,
			Src: uint64(uintptr(unsafe.Pointer(&page[0]))),
			Len: pageSize,
		}
		if err := ioctl(fd, uffdioCopyIoctl, unsafe.Pointer(&copyArg)); err != nil {
			// Spurious/duplicate faults can race other vCPUs; treat
			// them as benign and keep serving.
			if errors.Is(err, syscall.EEXIST) || errors.Is(err, syscall.EAGAIN) {
				return nil
			}
			return fmt.Errorf("uffd: UFFDIO_COPY: %w", err)
		}
		if s.cfg.RecordHotPages {
			s.hotPages.Add(HotPage{Region: uint32(idx), PageOffset: regionOff})
		}
		return nil
	}
	return fmt.Errorf("uffd: fault addr 0x%x outside any registered region", addr)
}

// prefetchInto walks list and issues a UFFDIO_COPY for each entry
// against the supplied fork's userfaultfd. It tolerates EEXIST/EAGAIN
// the same way the fault handler does so a racing first-touch fault
// from a vCPU does not abort the whole prefetch.
func (s *Server) prefetchInto(fd int, regions []MemoryRegion, list *HotPageList) error {
	if list == nil {
		return nil
	}
	pages := list.Snapshot()
	if len(pages) == 0 {
		return nil
	}
	page := make([]byte, s.pageSize)
	pageSize := uint64(s.pageSize)
	for _, hp := range pages {
		if int(hp.Region) >= len(regions) {
			return fmt.Errorf("uffd: prefetch entry refers to region %d (only %d registered)", hp.Region, len(regions))
		}
		r := regions[hp.Region]
		if hp.PageOffset+pageSize > r.Size {
			return fmt.Errorf("uffd: prefetch offset %d outside region %d size %d", hp.PageOffset, hp.Region, r.Size)
		}
		dst := uint64(r.BaseHostAddr) + hp.PageOffset
		src := int64(r.MemFileOffset + hp.PageOffset)
		if _, err := s.memFile.ReadAt(page, src); err != nil && !errors.Is(err, io.EOF) {
			return fmt.Errorf("uffd: prefetch read template at %d: %w", src, err)
		}
		copyArg := uffdioCopyArg{
			Dst: dst,
			Src: uint64(uintptr(unsafe.Pointer(&page[0]))),
			Len: pageSize,
		}
		if err := ioctl(fd, uffdioCopyIoctl, unsafe.Pointer(&copyArg)); err != nil {
			if errors.Is(err, syscall.EEXIST) || errors.Is(err, syscall.EAGAIN) {
				continue
			}
			return fmt.Errorf("uffd: prefetch UFFDIO_COPY: %w", err)
		}
	}
	return nil
}

func ioctl(fd int, req uintptr, arg unsafe.Pointer) error {
	_, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), req, uintptr(arg))
	if errno != 0 {
		return errno
	}
	return nil
}
