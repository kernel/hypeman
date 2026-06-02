//go:build linux

package uffdpager

import (
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/go-chi/chi/v5"
)

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

	activeFaults        atomic.Int64
	maxConcurrentFaults atomic.Int64
	faultNanos          atomic.Int64
	faultMaxNanos       atomic.Int64
	readPageNanos       atomic.Int64
	readPageMaxNanos    atomic.Int64
	backingReadNanos    atomic.Int64
	backingReadMaxNanos atomic.Int64
	copyNanos           atomic.Int64
	copyMaxNanos        atomic.Int64
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

func Main(args []string) error {
	fs := flag.NewFlagSet("hypeman-uffd-pager", flag.ContinueOnError)
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
