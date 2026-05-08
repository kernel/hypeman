// Package uffd implements a userfaultfd page server for firecracker
// snapshot fan-out. The server backs many concurrent forks against a
// single read-only template mem-file: instead of letting firecracker
// mmap the mem-file privately per fork (which forces every page to be
// copied on first touch), firecracker is configured to use a
// userfaultfd memory backend, and this server populates pages on
// demand from the template file.
//
// One Server instance handles one template mem-file and any number of
// fork connections. Each fork's firecracker process connects to a
// per-fork UDS and hands the server its userfaultfd via SCM_RIGHTS
// alongside a JSON payload describing the guest memory mappings; the
// server then handles UFFDIO_COPY for every faulted page.
//
// The protocol (firecracker_uffd_protocol below) is the contract
// firecracker speaks; we keep it isolated here so PR 8 can ride on
// top to prefetch hot pages without touching firecracker glue code.
//
// PR 5 ships the server skeleton, the protocol parser, and a unit
// test surface that doesn't require KVM. The hot-path syscalls live
// in server_linux.go behind a build tag because userfaultfd is a
// Linux-only kernel feature.
package uffd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// ErrUnsupported is returned on platforms where userfaultfd is not
// available. Callers should treat this as "fall back to mmap MAP_PRIVATE."
var ErrUnsupported = errors.New("userfaultfd unsupported on this platform")

// MemoryRegion describes a contiguous region of guest physical memory
// that maps into a [BaseHostAddr, BaseHostAddr+Size) virtual range in
// the firecracker process. The server services UFFDIO_COPY into that
// range using bytes from MemFileOffset.
type MemoryRegion struct {
	BaseHostAddr  uintptr `json:"base_host_virt_addr"`
	Size          uint64  `json:"size"`
	MemFileOffset uint64  `json:"offset"`
}

// firecrackerHandshake is the JSON payload firecracker sends on its
// UDS connection right before it passes the userfaultfd via SCM_RIGHTS.
// We only use the fields we care about for serving page faults; the
// rest of firecracker's payload is ignored.
type firecrackerHandshake struct {
	Mappings []MemoryRegion `json:"mappings"`
}

// Config configures a Server.
type Config struct {
	// MemFilePath is the path to the template mem-file. The server
	// opens it read-only and serves pages from it.
	MemFilePath string

	// SocketDir is where per-fork UDS files live. The directory must
	// exist and be writable by the server. One UDS is created per
	// RegisterFork call.
	SocketDir string

	// PageSize is the target page size for UFFDIO_COPY. Must be a
	// multiple of os.Getpagesize. Zero means use the host page size.
	PageSize int

	// RecordHotPages turns on per-fault recording. Every successfully
	// served page is appended to the server's hot-page list. Callers
	// typically enable this during a template's first warmup fork,
	// then HotPages().Save() the result before promoting the template.
	RecordHotPages bool
}

// Server owns the template mem-file and dispatches userfaultfd events
// for every connected fork. It is safe for concurrent use; methods may
// be called from any goroutine.
type Server struct {
	cfg     Config
	memFile *os.File
	memSize int64

	mu       sync.Mutex
	listens  map[string]*forkListen // forkID -> per-fork bookkeeping
	closed   bool
	pageSize int

	hotPages HotPageList // recorded faults; only used when cfg.RecordHotPages
}

type forkListen struct {
	socketPath string
	closer     func() error

	// prefetch is set by the platform-specific listener once the uffd
	// fd has been received and registered. Calling it issues UFFDIO_COPY
	// for every entry in the supplied list against the fork's uffd.
	// Nil means the fork hasn't connected yet.
	prefetch func(*HotPageList) error
}

// NewServer opens the template mem-file and prepares the server. It
// does not start any goroutines yet; callers register forks one by one.
// When the server is closed, the mem-file fd is released; in-flight
// fork handlers are signaled to exit and joined.
func NewServer(cfg Config) (*Server, error) {
	if cfg.MemFilePath == "" {
		return nil, errors.New("uffd: MemFilePath is required")
	}
	if cfg.SocketDir == "" {
		return nil, errors.New("uffd: SocketDir is required")
	}
	if err := os.MkdirAll(cfg.SocketDir, 0o755); err != nil {
		return nil, fmt.Errorf("uffd: ensure socket dir: %w", err)
	}
	f, err := os.Open(cfg.MemFilePath)
	if err != nil {
		return nil, fmt.Errorf("uffd: open mem-file: %w", err)
	}
	st, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("uffd: stat mem-file: %w", err)
	}
	pageSize := cfg.PageSize
	if pageSize == 0 {
		pageSize = os.Getpagesize()
	}
	return &Server{
		cfg:      cfg,
		memFile:  f,
		memSize:  st.Size(),
		listens:  map[string]*forkListen{},
		pageSize: pageSize,
	}, nil
}

// SocketPath returns the UDS path that should be passed to firecracker
// for a fork. RegisterFork must be called first.
func (s *Server) SocketPath(forkID string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return "", errors.New("uffd: server closed")
	}
	listen, ok := s.listens[forkID]
	if !ok {
		return "", fmt.Errorf("uffd: fork %q is not registered", forkID)
	}
	return listen.socketPath, nil
}

// MemSize returns the size of the template mem-file in bytes. Useful
// for sizing prefetch buffers and validating handshake mappings.
func (s *Server) MemSize() int64 { return s.memSize }

// PageSize returns the configured page size in bytes.
func (s *Server) PageSize() int { return s.pageSize }

// HotPages returns the server's hot-page recorder. The returned value
// is the live list — Add/Snapshot/Save are all valid. Recording only
// happens when Config.RecordHotPages is set; callers may still inspect
// the (empty) list otherwise.
func (s *Server) HotPages() *HotPageList { return &s.hotPages }

// Prefetch issues UFFDIO_COPY for every entry in list against the fork
// identified by forkID. Used to warm up known-hot pages before the
// guest unpauses, eliminating fault round-trips for anything we've
// pre-recorded. Returns an error if the fork is unknown or hasn't
// connected yet, or if the underlying ioctl fails (other than the
// benign EEXIST/EAGAIN race noted in copyPageForFault).
func (s *Server) Prefetch(forkID string, list *HotPageList) error {
	if list == nil || list.Len() == 0 {
		return nil
	}
	s.mu.Lock()
	listen, ok := s.listens[forkID]
	prefetch := func(*HotPageList) error { return nil }
	if ok && listen.prefetch != nil {
		prefetch = listen.prefetch
	}
	s.mu.Unlock()
	if !ok {
		return fmt.Errorf("uffd: fork %q is not registered", forkID)
	}
	if listen.prefetch == nil {
		return fmt.Errorf("uffd: fork %q has not yet connected", forkID)
	}
	return prefetch(list)
}

// installPrefetcher is called by the platform-specific listener once
// the uffd is ready. It is a no-op if the fork has been unregistered.
func (s *Server) installPrefetcher(forkID string, fn func(*HotPageList) error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	listen, ok := s.listens[forkID]
	if !ok {
		return
	}
	listen.prefetch = fn
}

// Close stops the server, closes all per-fork listeners, and releases
// the template mem-file fd. After Close returns, the server cannot be
// reused.
func (s *Server) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	listens := s.listens
	s.listens = nil
	s.mu.Unlock()

	var firstErr error
	for _, l := range listens {
		if l.closer != nil {
			if err := l.closer(); err != nil && firstErr == nil {
				firstErr = err
			}
		}
	}
	if err := s.memFile.Close(); err != nil && firstErr == nil {
		firstErr = err
	}
	return firstErr
}

// parseHandshake decodes firecracker's JSON handshake payload. Exposed
// so tests can validate the parser without spinning up a real socket.
func parseHandshake(data []byte) (firecrackerHandshake, error) {
	var h firecrackerHandshake
	if err := json.Unmarshal(data, &h); err != nil {
		return firecrackerHandshake{}, fmt.Errorf("uffd: parse handshake: %w", err)
	}
	if len(h.Mappings) == 0 {
		return firecrackerHandshake{}, errors.New("uffd: handshake has no mappings")
	}
	return h, nil
}

// resolveSocketPath returns the per-fork socket path. The server uses
// short names because Unix domain sockets have a tight sun_path limit;
// callers should keep SocketDir short.
func (s *Server) resolveSocketPath(forkID string) string {
	return filepath.Join(s.cfg.SocketDir, forkID+".uffd")
}

// RegisterFork allocates a per-fork listener and waits asynchronously
// for firecracker to connect. The returned context cancels when the
// server closes or the fork unregisters.
//
// On Linux the heavy lifting (accept, recvmsg, ioctl loop) lives in
// server_linux.go; on other platforms RegisterFork returns ErrUnsupported.
func (s *Server) RegisterFork(ctx context.Context, forkID string) (string, error) {
	if forkID == "" {
		return "", errors.New("uffd: fork id is required")
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return "", errors.New("uffd: server closed")
	}
	if _, dup := s.listens[forkID]; dup {
		s.mu.Unlock()
		return "", fmt.Errorf("uffd: fork %q already registered", forkID)
	}
	socketPath := s.resolveSocketPath(forkID)
	s.mu.Unlock()

	closer, err := s.startListener(ctx, forkID, socketPath)
	if err != nil {
		return "", err
	}

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		_ = closer()
		return "", errors.New("uffd: server closed during register")
	}
	s.listens[forkID] = &forkListen{socketPath: socketPath, closer: closer}
	s.mu.Unlock()

	return socketPath, nil
}

// UnregisterFork closes the listener for forkID. Called when the fork
// is destroyed; the server stops servicing its faults and removes the
// UDS file.
func (s *Server) UnregisterFork(forkID string) error {
	s.mu.Lock()
	listen, ok := s.listens[forkID]
	if !ok {
		s.mu.Unlock()
		return nil
	}
	delete(s.listens, forkID)
	s.mu.Unlock()
	if listen.closer != nil {
		return listen.closer()
	}
	return nil
}
