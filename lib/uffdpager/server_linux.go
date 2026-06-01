//go:build linux

package uffdpager

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	"github.com/go-chi/chi/v5"
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

type server struct {
	dataDir       string
	versionKey    string
	cache         *PageCache
	controlSocket string
	sessionRoot   string
	httpServer    *http.Server

	mu       sync.Mutex
	sessions map[string]*session
	draining bool

	faults           atomic.Int64
	overlayFaults    atomic.Int64
	backingBytesRead atomic.Int64
	copies           atomic.Int64
	copyErrors       atomic.Int64
}

type session struct {
	id                string
	instanceID        string
	backingMemoryPath string
	cacheKey          string
	socketPath        string
	listener          *net.UnixListener
	backingFile       *os.File
	overlays          map[int64][]byte
	server            *server
	done              chan struct{}
	closeOnce         sync.Once
	uffdFD            int
	conn              *net.UnixConn
}

type uffdEvent struct {
	kind byte
	addr int64
}

func Main(args []string) error {
	fs := flag.NewFlagSet("internal-uffd-pager", flag.ContinueOnError)
	dataDir := fs.String("data-dir", "", "hypeman data directory")
	versionKey := fs.String("version-key", "", "pager version key")
	cacheMaxBytes := fs.Int64("cache-max-bytes", defaultCacheMaxBytes, "maximum shared page cache bytes")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if strings.TrimSpace(*dataDir) == "" {
		return fmt.Errorf("--data-dir is required")
	}
	if strings.TrimSpace(*versionKey) == "" {
		return fmt.Errorf("--version-key is required")
	}

	s := newServer(*dataDir, *versionKey, *cacheMaxBytes)
	return s.run()
}

func newServer(dataDir, versionKey string, cacheMaxBytes int64) *server {
	dir := pagerVersionDir(dataDir, versionKey)
	return &server{
		dataDir:       dataDir,
		versionKey:    versionKey,
		cache:         NewPageCache(cacheMaxBytes),
		controlSocket: filepath.Join(dir, controlSocketFile),
		sessionRoot:   filepath.Join(dir, sessionsDir),
		sessions:      make(map[string]*session),
	}
}

func (s *server) run() error {
	if err := os.MkdirAll(s.sessionRoot, 0755); err != nil {
		return fmt.Errorf("create uffd session directory: %w", err)
	}
	_ = os.Remove(s.controlSocket)
	listener, err := net.Listen("unix", s.controlSocket)
	if err != nil {
		return fmt.Errorf("listen on uffd control socket: %w", err)
	}
	defer listener.Close()

	_ = os.WriteFile(filepath.Join(filepath.Dir(s.controlSocket), pagerPIDFile), []byte(fmt.Sprintf("%d\n", os.Getpid())), 0644)

	router := chi.NewRouter()
	router.Get("/health", s.handleHealth)
	router.Get("/stats", s.handleStats)
	router.Post("/sessions", s.handleCreateSession)
	router.Post("/sessions/{id}/close", s.handleCloseSession)
	router.Post("/drain", s.handleDrain)

	s.httpServer = &http.Server{Handler: router}
	err = s.httpServer.Serve(listener)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func (s *server) handleHealth(w http.ResponseWriter, r *http.Request) {
	s.writeJSON(w, http.StatusOK, HealthResponse{
		Version:        s.versionKey,
		Draining:       s.isDraining(),
		ActiveSessions: s.activeSessions(),
	})
}

func (s *server) handleStats(w http.ResponseWriter, r *http.Request) {
	cacheBytes, cacheMax, cacheItems, hits, misses := s.cache.SnapshotStats()
	s.writeJSON(w, http.StatusOK, Stats{
		Version:          s.versionKey,
		Draining:         s.isDraining(),
		ActiveSessions:   s.activeSessions(),
		CacheBytes:       cacheBytes,
		CacheMax:         cacheMax,
		CacheItems:       cacheItems,
		CacheHits:        hits,
		CacheMisses:      misses,
		Faults:           s.faults.Load(),
		OverlayFaults:    s.overlayFaults.Load(),
		BackingBytesRead: s.backingBytesRead.Load(),
		Copies:           s.copies.Load(),
		CopyErrors:       s.copyErrors.Load(),
	})
}

func (s *server) handleCreateSession(w http.ResponseWriter, r *http.Request) {
	if s.isDraining() {
		http.Error(w, "pager is draining", http.StatusServiceUnavailable)
		return
	}

	var req CreateSessionRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		http.Error(w, fmt.Sprintf("decode request: %v", err), http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(req.SessionID) == "" {
		req.SessionID = req.InstanceID
	}
	if strings.TrimSpace(req.SessionID) == "" {
		http.Error(w, "session_id or instance_id is required", http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(req.BackingMemoryPath) == "" {
		http.Error(w, "backing_memory_path is required", http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(req.CacheKey) == "" {
		http.Error(w, "cache_key is required", http.StatusBadRequest)
		return
	}

	created, err := s.createSession(req)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	s.writeJSON(w, http.StatusOK, CreateSessionResponse{
		SessionID:      created.id,
		UFFDSocketPath: created.socketPath,
		PagerVersion:   s.versionKey,
	})
}

func (s *server) handleCloseSession(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	s.closeSession(id)
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) handleDrain(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.draining = true
	shouldExit := len(s.sessions) == 0
	s.mu.Unlock()

	if shouldExit {
		s.shutdownSoon()
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) createSession(req CreateSessionRequest) (*session, error) {
	id := sanitizeSessionID(req.SessionID)
	socketPath := filepath.Join(s.sessionRoot, id+".sock")
	_ = os.Remove(socketPath)
	addr := net.UnixAddr{Name: socketPath, Net: "unix"}
	listener, err := net.ListenUnix("unix", &addr)
	if err != nil {
		return nil, fmt.Errorf("listen for uffd session %s: %w", id, err)
	}

	overlays := make(map[int64][]byte, len(req.Overlays))
	for _, overlay := range req.Overlays {
		if overlay.GuestMemoryOffset < 0 || strings.TrimSpace(overlay.Path) == "" {
			listener.Close()
			return nil, fmt.Errorf("invalid overlay page: offset=%d path=%q", overlay.GuestMemoryOffset, overlay.Path)
		}
		data, err := os.ReadFile(overlay.Path)
		if err != nil {
			listener.Close()
			return nil, fmt.Errorf("read overlay page %q: %w", overlay.Path, err)
		}
		overlays[overlay.GuestMemoryOffset] = data
	}

	sess := &session{
		id:                id,
		instanceID:        req.InstanceID,
		backingMemoryPath: req.BackingMemoryPath,
		cacheKey:          req.CacheKey,
		socketPath:        socketPath,
		listener:          listener,
		overlays:          overlays,
		server:            s,
		done:              make(chan struct{}),
		uffdFD:            -1,
	}

	s.mu.Lock()
	if existing := s.sessions[id]; existing != nil {
		existing.close()
	}
	s.sessions[id] = sess
	s.mu.Unlock()

	go sess.run()
	return sess, nil
}

func (s *server) closeSession(id string) {
	id = sanitizeSessionID(id)
	s.mu.Lock()
	sess := s.sessions[id]
	delete(s.sessions, id)
	shouldExit := s.draining && len(s.sessions) == 0
	s.mu.Unlock()
	if sess != nil {
		sess.close()
	}
	if shouldExit {
		s.shutdownSoon()
	}
}

func (s *server) removeSession(sess *session) {
	s.mu.Lock()
	if current := s.sessions[sess.id]; current == sess {
		delete(s.sessions, sess.id)
	}
	shouldExit := s.draining && len(s.sessions) == 0
	s.mu.Unlock()
	if shouldExit {
		s.shutdownSoon()
	}
}

func (s *server) shutdownSoon() {
	go func() {
		time.Sleep(50 * time.Millisecond)
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = s.httpServer.Shutdown(ctx)
	}()
}

func (s *server) activeSessions() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.sessions)
}

func (s *server) isDraining() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.draining
}

func (s *server) writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(value); err != nil {
		log.Printf("write json response: %v", err)
	}
}

func (s *session) run() {
	defer func() {
		s.close()
		s.server.removeSession(s)
	}()

	conn, err := s.listener.AcceptUnix()
	if err != nil {
		return
	}
	s.conn = conn

	file, err := os.Open(s.backingMemoryPath)
	if err != nil {
		log.Printf("uffd session %s open backing memory: %v", s.id, err)
		return
	}
	s.backingFile = file

	mappings, uffdFD, err := recvMappingsAndFD(conn)
	if err != nil {
		log.Printf("uffd session %s receive mappings and fd: %v", s.id, err)
		return
	}
	s.uffdFD = uffdFD

	sort.Slice(mappings, func(i, j int) bool {
		return mappings[i].BaseHostVirtAddr < mappings[j].BaseHostVirtAddr
	})
	s.handleFaults(mappings)
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
	mapping, pageAddr, pageOffset, pageSize, ok := findMapping(mappings, faultAddr)
	if !ok {
		return fmt.Errorf("fault address %#x outside guest mappings", faultAddr)
	}
	_ = mapping

	s.server.faults.Add(1)
	page, overlay, err := s.readPage(pageOffset, pageSize)
	if err != nil {
		return err
	}
	if overlay {
		s.server.overlayFaults.Add(1)
	}
	if err := uffdCopy(s.uffdFD, uint64(pageAddr), page); err != nil {
		return err
	}
	s.server.copies.Add(1)
	return nil
}

func (s *session) readPage(offset int64, size int) ([]byte, bool, error) {
	if page, ok := s.overlays[offset]; ok {
		if len(page) != size {
			return nil, true, fmt.Errorf("overlay page at offset %d has size %d, expected %d", offset, len(page), size)
		}
		return append([]byte(nil), page...), true, nil
	}
	if page, ok := s.server.cache.Get(s.cacheKey, offset, size); ok {
		return page, false, nil
	}

	page := make([]byte, size)
	n, err := s.backingFile.ReadAt(page, offset)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, false, fmt.Errorf("read backing page at %d: %w", offset, err)
	}
	if n > 0 {
		s.server.backingBytesRead.Add(int64(n))
	}
	s.server.cache.Add(s.cacheKey, offset, page)
	return page, false, nil
}

func (s *session) close() {
	s.closeOnce.Do(func() {
		if s.listener != nil {
			_ = s.listener.Close()
		}
		if s.conn != nil {
			_ = s.conn.Close()
		}
		if s.uffdFD >= 0 {
			_ = unix.Close(s.uffdFD)
		}
		if s.backingFile != nil {
			_ = s.backingFile.Close()
		}
		_ = os.Remove(s.socketPath)
		close(s.done)
	})
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

func sanitizeSessionID(id string) string {
	id = strings.TrimSpace(id)
	if id == "" {
		return fmt.Sprintf("session-%d", time.Now().UnixNano())
	}
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z':
			return r
		case r >= 'A' && r <= 'Z':
			return r
		case r >= '0' && r <= '9':
			return r
		case r == '-', r == '_', r == '.':
			return r
		default:
			return '-'
		}
	}, id)
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
