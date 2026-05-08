package cloudhypervisor

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const serialSocketConnectTimeout = 5 * time.Second

type serialSocketLogger struct {
	conn net.Conn
	file *os.File
	done chan struct{}
	once sync.Once
}

func startSerialSocketLogger(ctx context.Context, socketPath, logPath string) (*serialSocketLogger, error) {
	if err := os.MkdirAll(filepath.Dir(logPath), 0755); err != nil {
		return nil, fmt.Errorf("create serial log directory: %w", err)
	}

	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return nil, fmt.Errorf("open serial log: %w", err)
	}

	conn, err := dialSerialSocket(ctx, socketPath)
	if err != nil {
		logFile.Close()
		return nil, err
	}

	logger := &serialSocketLogger{
		conn: conn,
		file: logFile,
		done: make(chan struct{}),
	}
	go logger.copy()
	return logger, nil
}

func dialSerialSocket(ctx context.Context, socketPath string) (net.Conn, error) {
	ctx, cancel := context.WithTimeout(ctx, serialSocketConnectTimeout)
	defer cancel()

	dialer := net.Dialer{}
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()

	var lastErr error
	for {
		conn, err := dialer.DialContext(ctx, "unix", socketPath)
		if err == nil {
			return conn, nil
		}
		lastErr = err

		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("connect serial socket %s: %w", socketPath, lastErr)
		case <-ticker.C:
		}
	}
}

func (l *serialSocketLogger) copy() {
	defer close(l.done)
	_, _ = io.Copy(l.file, l.conn)
}

// Close terminates the serial logger. It closes the connection (unblocking
// io.Copy), waits for the copy goroutine to finish, then closes the log file.
// Safe to call on a nil receiver and idempotent.
func (l *serialSocketLogger) Close() {
	if l == nil {
		return
	}
	l.once.Do(func() {
		_ = l.conn.Close()
		<-l.done
		_ = l.file.Close()
	})
}
