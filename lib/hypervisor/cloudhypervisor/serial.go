package cloudhypervisor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"syscall"

	"github.com/kernel/hypeman/lib/logger"
)

// serialReader owns the listener for the per-instance unix socket that
// Cloud Hypervisor connects to for serial output, and copies bytes from
// the connection into the serial log file with O_APPEND. Owning the
// writer fd lets copytruncate log rotation work safely: O_APPEND
// atomically seeks to EOF on every write, so writes after a truncate
// land at byte 0 instead of the writer's stale offset.
type serialReader struct {
	ln         net.Listener
	socketPath string
	logPath    string
	done       chan struct{}
}

// startSerialReader binds the unix socket at socketPath, removing any
// stale predecessor, and starts a goroutine that accepts CH's connection
// and pipes serial output into logPath. The listener is closed after the
// first accept (CH only ever opens one connection per VM). Callers must
// call Close on failure paths to release the listener if CH never
// connects.
func startSerialReader(ctx context.Context, socketPath, logPath string) (*serialReader, error) {
	if socketPath == "" || logPath == "" {
		return nil, errors.New("serial: socket and log path are required")
	}

	if err := os.MkdirAll(filepath.Dir(logPath), 0755); err != nil {
		return nil, fmt.Errorf("serial: create log dir: %w", err)
	}

	if err := os.Remove(socketPath); err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("serial: remove stale socket: %w", err)
	}

	ln, err := net.Listen("unix", socketPath)
	if err != nil {
		return nil, fmt.Errorf("serial: listen %s: %w", socketPath, err)
	}

	sr := &serialReader{
		ln:         ln,
		socketPath: socketPath,
		logPath:    logPath,
		done:       make(chan struct{}),
	}
	go sr.run(ctx)
	return sr, nil
}

func (s *serialReader) run(ctx context.Context) {
	defer close(s.done)
	defer os.Remove(s.socketPath)

	conn, err := s.ln.Accept()
	// CH only opens one connection per VM, so close the listener now.
	_ = s.ln.Close()
	if err != nil {
		// Closed before CH connected (e.g. boot failed) — nothing to do.
		return
	}
	defer conn.Close()

	f, err := os.OpenFile(s.logPath, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0644)
	if err != nil {
		logger.FromContext(ctx).ErrorContext(ctx, "serial: open log",
			"path", s.logPath, "err", err)
		return
	}
	defer f.Close()

	if _, err := io.Copy(f, conn); err != nil &&
		!errors.Is(err, io.EOF) &&
		!errors.Is(err, net.ErrClosed) &&
		!errors.Is(err, syscall.EPIPE) {
		logger.FromContext(ctx).WarnContext(ctx, "serial: copy ended with error",
			"path", s.logPath, "err", err)
	}
}

// Close releases the listener if it is still open. Safe to call multiple
// times. Once CH has connected and closed the socket the goroutine exits
// on its own; Close is a best-effort signal for the failure path where
// CH never boots.
func (s *serialReader) Close() {
	if s == nil {
		return
	}
	_ = s.ln.Close()
}
