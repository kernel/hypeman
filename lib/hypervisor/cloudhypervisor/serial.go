package cloudhypervisor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/kernel/hypeman/lib/logger"
)

// serialReader connects to the per-instance unix socket that Cloud
// Hypervisor binds for serial output (mode=Socket), and copies bytes
// from that connection into the serial log file with O_APPEND. Owning
// the writer fd lets copytruncate log rotation work safely: O_APPEND
// atomically seeks to EOF on every write, so writes after a truncate
// land at byte 0 instead of the writer's stale offset.
//
// CH is the server here — it calls UnixListener::bind(socket) during
// VM creation. Hypeman is the client and dials with retry, since the
// reader is started before CH has had a chance to bind.
type serialReader struct {
	socketPath string
	logPath    string
	logFile    *os.File

	cancel context.CancelFunc
	done   chan struct{}

	mu   sync.Mutex
	conn net.Conn
}

// startSerialReader removes any stale socket left over from a prior
// run, opens the log file with O_APPEND, and spawns a goroutine that
// dials the socket once CH binds it and pipes serial output into the
// log. Callers must call Close on failure paths and on shutdown.
func startSerialReader(ctx context.Context, socketPath, logPath string) (*serialReader, error) {
	if socketPath == "" || logPath == "" {
		return nil, errors.New("serial: socket and log path are required")
	}

	if err := os.MkdirAll(filepath.Dir(logPath), 0755); err != nil {
		return nil, fmt.Errorf("serial: create log dir: %w", err)
	}

	// CH refuses to start if the socket path already exists (it calls
	// bind() on it). Wipe any leftover from a prior run.
	if err := os.Remove(socketPath); err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("serial: remove stale socket: %w", err)
	}

	f, err := os.OpenFile(logPath, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0644)
	if err != nil {
		return nil, fmt.Errorf("serial: open log: %w", err)
	}

	runCtx, cancel := context.WithCancel(context.Background())
	sr := &serialReader{
		socketPath: socketPath,
		logPath:    logPath,
		logFile:    f,
		cancel:     cancel,
		done:       make(chan struct{}),
	}
	go sr.run(runCtx, logger.FromContext(ctx))
	return sr, nil
}

func (s *serialReader) run(ctx context.Context, log *slog.Logger) {
	defer close(s.done)
	defer s.logFile.Close()
	// Drop the socket file when the goroutine exits so it doesn't persist
	// on disk after the VM is gone, regardless of whether Close was
	// called explicitly. CH normally unlinks the path itself on Drop,
	// but this is belt-and-suspenders for the leak-on-success case.
	defer func() { _ = os.Remove(s.socketPath) }()

	conn, err := dialUnixWithRetry(ctx, s.socketPath, 60*time.Second)
	if err != nil {
		// Caller closed us before CH bound the socket, or CH never
		// did. Either way, nothing more to do.
		return
	}
	// Re-check ctx under the lock before publishing the conn. If Close
	// fired between dial returning and us taking the lock, it would have
	// observed s.conn == nil and given up — without this check we'd
	// publish the conn afterward and io.Copy would block forever.
	s.mu.Lock()
	if ctx.Err() != nil {
		s.mu.Unlock()
		_ = conn.Close()
		return
	}
	s.conn = conn
	s.mu.Unlock()

	if _, err := io.Copy(s.logFile, conn); err != nil &&
		!errors.Is(err, io.EOF) &&
		!errors.Is(err, net.ErrClosed) &&
		!errors.Is(err, syscall.EPIPE) {
		log.WarnContext(ctx, "serial: copy ended with error",
			"path", s.logPath, "err", err)
	}
	_ = conn.Close()
}

// dialUnixWithRetry polls for the CH listener to come up. The socket
// path is created by CH inside vm.create, which happens after the
// reader is started, so we must wait. Returns the first successful
// connection or the last dial error after timeout.
func dialUnixWithRetry(ctx context.Context, path string, timeout time.Duration) (net.Conn, error) {
	deadline := time.Now().Add(timeout)
	var dialer net.Dialer
	var lastErr error
	for {
		if err := ctx.Err(); err != nil {
			if lastErr != nil {
				return nil, lastErr
			}
			return nil, err
		}
		conn, err := dialer.DialContext(ctx, "unix", path)
		if err == nil {
			return conn, nil
		}
		lastErr = err
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("serial: dial %s: %w", path, lastErr)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// Close stops the reader. Safe to call multiple times.
func (s *serialReader) Close() {
	if s == nil {
		return
	}
	s.cancel()
	s.mu.Lock()
	if s.conn != nil {
		_ = s.conn.Close()
	}
	s.mu.Unlock()
	select {
	case <-s.done:
	case <-time.After(2 * time.Second):
	}
	_ = os.Remove(s.socketPath)
}
