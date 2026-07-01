//go:build linux

package uffdpager

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"unsafe"

	"github.com/go-chi/chi/v5"
	"golang.org/x/sys/unix"
)

// _UFFDIO_UNREGISTER ioctl. Derived the same way as uffdioCopy in
// server_faults_linux.go: _IOR(UFFDIO=0xAA, _UFFDIO_UNREGISTER=1, struct
// uffdio_range{u64 start; u64 len}) on the asm-generic ioctl encoding.
const uffdioUnregister = 0x8010aa01

type uffdioRange struct {
	start uint64
	len   uint64
}

// completeRequest is a single /complete attempt handed to the fault loop. ctx
// is the HTTP request context: when the caller times out or disconnects the
// populate aborts and the session keeps serving, so the control plane's session
// binding stays accurate and the attempt can simply be retried. The reply
// channel is buffered so the fault loop never blocks delivering a result even
// if the caller has already given up.
type completeRequest struct {
	ctx  context.Context
	resp chan error
}

// handleComplete drives an existing session to fully populate its guest memory
// from the backing file and detach userfaultfd, so the restored VM stops
// depending on this pager. The VM keeps running throughout.
func (s *server) handleComplete(w http.ResponseWriter, r *http.Request) {
	id := sanitizeSessionID(chi.URLParam(r, "id"))
	s.mu.Lock()
	sess := s.sessions[id]
	s.mu.Unlock()
	if sess == nil {
		http.Error(w, "session not found", http.StatusNotFound)
		return
	}

	req := &completeRequest{ctx: r.Context(), resp: make(chan error, 1)}
	select {
	case sess.completeReqCh <- req:
	case <-sess.done:
		http.Error(w, "session closed", http.StatusConflict)
		return
	default:
		http.Error(w, "completion already in progress", http.StatusConflict)
		return
	}
	sess.wake()

	select {
	case err := <-req.resp:
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	case <-sess.done:
		http.Error(w, "session closed before completion", http.StatusInternalServerError)
	case <-r.Context().Done():
		http.Error(w, r.Context().Err().Error(), http.StatusGatewayTimeout)
	}
}

// wake writes the wake pipe so the fault loop breaks out of an idle poll and
// notices a pending completion request. It takes wakeMu so it never writes to
// a pipe that close() already closed (and whose fd number may be recycled).
func (s *session) wake() {
	s.wakeMu.Lock()
	defer s.wakeMu.Unlock()
	if s.wakeW >= 0 {
		var b [1]byte
		_, _ = unix.Write(s.wakeW, b[:])
	}
}

// takeCompletion runs a queued completion request if one is pending. It returns
// true only when the session fully completed and the fault loop should exit so
// the session tears down; on failure the session keeps serving faults so the VM
// stays safely backed by the pager.
func (s *session) takeCompletion() bool {
	select {
	case req := <-s.completeReqCh:
		err := s.completeAndUnregister(req.ctx)
		req.resp <- err
		return err == nil
	default:
		return false
	}
}

// completeAndUnregister populates every page from the backing file and only then
// unregisters the ranges. Unregister must happen strictly after a full populate:
// once a range is unregistered the kernel zero-fills any page that was still
// absent, which would be corruption. If the populate fails or the caller gives
// up, the ranges are left registered so the fault loop can keep serving them.
func (s *session) completeAndUnregister(ctx context.Context) error {
	if err := s.populateAll(ctx); err != nil {
		return err
	}
	// The caller records the detach only after a successful response. If it has
	// already given up, abort before the point of no return so the session (and
	// the caller's record of it) stays intact for a retry.
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("completion abandoned before unregister: %w", err)
	}
	return s.unregisterAll()
}

func (s *session) populateAll(ctx context.Context) error {
	var firstErr error
	noteErr := func(err error) {
		if firstErr == nil {
			firstErr = err
		}
	}

	eventBuf := make([]byte, uffdMsgSize)
	for _, mapping := range s.mappings {
		pageSize := int(mapping.PageSize)
		if pageSize <= 0 {
			continue
		}
		page := make([]byte, pageSize)
		for off := uint64(0); off+uint64(pageSize) <= mapping.Size; off += uint64(pageSize) {
			if err := ctx.Err(); err != nil {
				return fmt.Errorf("populate abandoned: %w", err)
			}
			fileOffset := int64(mapping.Offset + off)
			n, err := s.backingFile.ReadAt(page, fileOffset)
			if n != pageSize {
				noteErr(fmt.Errorf("read backing page at %d: read %d/%d bytes: %w", fileOffset, n, pageSize, err))
				continue
			}
			s.server.backingBytesRead.Add(int64(n))
			if err := s.populatePage(mapping.BaseHostVirtAddr+off, page, eventBuf); err != nil {
				s.server.copyErrors.Add(1)
				noteErr(fmt.Errorf("populate page at %#x: %w", mapping.BaseHostVirtAddr+off, err))
			}
		}
	}
	return firstErr
}

func (s *session) unregisterAll() error {
	var firstErr error
	for _, mapping := range s.mappings {
		// EINVAL means the range is already unregistered, which makes a retried
		// completion idempotent.
		if err := uffdUnregister(s.uffdFD, mapping.BaseHostVirtAddr, mapping.Size); err != nil && !errors.Is(err, unix.EINVAL) {
			if firstErr == nil {
				firstErr = fmt.Errorf("unregister range %#x len %d: %w", mapping.BaseHostVirtAddr, mapping.Size, err)
			}
		}
	}
	return firstErr
}

// populatePage copies one page into guest memory. UFFDIO_COPY returns EEXIST
// (treated as success) for pages the guest already faulted in, and can return
// EAGAIN when a pending remove/unmap event from ballooning blocks the copy;
// draining those events and retrying clears it, mirroring the fault loop.
func (s *session) populatePage(dst uint64, page, eventBuf []byte) error {
	const maxAttempts = 5
	var lastErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		err := uffdCopy(s.uffdFD, dst, page)
		if err == nil {
			s.server.copies.Add(1)
			return nil
		}
		if !errors.Is(err, unix.EAGAIN) {
			return err
		}
		drainUFFDEvents(s.uffdFD, eventBuf)
		lastErr = err
	}
	return lastErr
}

// drainUFFDEvents discards pending events, including pagefaults. That is safe
// only because it runs during populateAll, which copies every page in every
// mapping: UFFDIO_COPY wakes the threads waiting on a discarded fault when the
// sweep reaches their page.
func drainUFFDEvents(fd int, buf []byte) {
	for {
		_, ok, err := readUFFDEvent(fd, buf)
		if err != nil || !ok {
			return
		}
	}
}

func drainWake(fd int) {
	var buf [16]byte
	for {
		n, err := unix.Read(fd, buf[:])
		if n <= 0 || err != nil {
			return
		}
	}
}

func newWakePipe() (int, int, error) {
	var fds [2]int
	if err := unix.Pipe2(fds[:], unix.O_NONBLOCK|unix.O_CLOEXEC); err != nil {
		return -1, -1, err
	}
	return fds[0], fds[1], nil
}

func uffdUnregister(fd int, start, length uint64) error {
	rng := uffdioRange{start: start, len: length}
	_, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), uintptr(uffdioUnregister), uintptr(unsafe.Pointer(&rng)))
	if errno != 0 {
		return errno
	}
	return nil
}

// CompleteSessionVersion asks the pager for the given version to fully populate
// and detach the session. It uses a context-bounded client with no fixed
// timeout because completion reads the whole backing image.
func (s *Supervisor) CompleteSessionVersion(ctx context.Context, versionKey, sessionID string) error {
	if s == nil || strings.TrimSpace(versionKey) == "" || strings.TrimSpace(sessionID) == "" {
		return nil
	}
	client := newUnixHTTPClient(pagerControlSocket(s.dataDir, versionKey), 0)
	path := "/sessions/" + urlPathEscape(sessionID) + "/complete"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://unix"+path, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode == http.StatusNotFound {
		return ErrSessionNotFound
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("uffd pager complete %s returned %s: %s", sessionID, resp.Status, strings.TrimSpace(string(data)))
	}
	return nil
}
