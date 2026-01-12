package ingress

import (
	"bufio"
	"context"
	"encoding/json"
	"log/slog"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/kernel/hypeman/lib/paths"
)

// CaddyLogForwarder tails Caddy's system log and forwards to OTEL.
type CaddyLogForwarder struct {
	paths  *paths.Paths
	logger *slog.Logger
	cmd    *exec.Cmd
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// NewCaddyLogForwarder creates a new log forwarder.
func NewCaddyLogForwarder(p *paths.Paths, logger *slog.Logger) *CaddyLogForwarder {
	return &CaddyLogForwarder{
		paths:  p,
		logger: logger,
	}
}

// Start begins tailing Caddy's log file and forwarding to OTEL.
func (f *CaddyLogForwarder) Start(ctx context.Context) error {
	ctx, f.cancel = context.WithCancel(ctx)

	// Caddy writes JSON logs to stderr, which daemon.go redirects to CaddyLogFile
	logPath := f.paths.CaddyLogFile()

	// Use tail -F (capital F) to follow file even if it's recreated
	f.cmd = exec.CommandContext(ctx, "tail", "-F", "-n", "0", logPath)

	stdout, err := f.cmd.StdoutPipe()
	if err != nil {
		return err
	}

	if err := f.cmd.Start(); err != nil {
		return err
	}

	f.wg.Add(1)
	go func() {
		defer f.wg.Done()
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			line := scanner.Text()
			f.forwardLogLine(ctx, line)
		}
	}()

	return nil
}

// Stop stops the log forwarder.
func (f *CaddyLogForwarder) Stop() {
	if f.cancel != nil {
		f.cancel()
	}
	if f.cmd != nil && f.cmd.Process != nil {
		if err := f.cmd.Process.Kill(); err != nil && f.logger != nil {
			f.logger.Debug("failed to kill tail process", "error", err)
		}
	}
	f.wg.Wait()
}

// caddyLogEntry represents a parsed Caddy JSON log entry.
type caddyLogEntry struct {
	Level   string  `json:"level"`
	TS      float64 `json:"ts"`
	Logger  string  `json:"logger"`
	Msg     string  `json:"msg"`
	Error   string  `json:"error,omitempty"`
	Module  string  `json:"module,omitempty"`
	Adapter string  `json:"adapter,omitempty"`
}

// forwardLogLine parses a JSON log line and forwards to OTEL logger.
func (f *CaddyLogForwarder) forwardLogLine(ctx context.Context, line string) {
	if f.logger == nil || line == "" {
		return
	}

	// Skip non-JSON lines (tail might output some status messages)
	if !strings.HasPrefix(line, "{") {
		return
	}

	var entry caddyLogEntry
	if err := json.Unmarshal([]byte(line), &entry); err != nil {
		// If we can't parse, log raw line at info level
		f.logger.InfoContext(ctx, "caddy: "+line)
		return
	}

	// Convert timestamp
	ts := time.Unix(int64(entry.TS), int64((entry.TS-float64(int64(entry.TS)))*1e9))

	// Build attributes
	attrs := []any{
		"caddy_logger", entry.Logger,
		"caddy_ts", ts.Format(time.RFC3339Nano),
	}
	if entry.Module != "" {
		attrs = append(attrs, "module", entry.Module)
	}
	if entry.Adapter != "" {
		attrs = append(attrs, "adapter", entry.Adapter)
	}
	if entry.Error != "" {
		attrs = append(attrs, "error", entry.Error)
	}

	// Forward with appropriate level
	msg := "caddy: " + entry.Msg
	switch strings.ToLower(entry.Level) {
	case "debug":
		f.logger.DebugContext(ctx, msg, attrs...)
	case "info":
		f.logger.InfoContext(ctx, msg, attrs...)
	case "warn":
		f.logger.WarnContext(ctx, msg, attrs...)
	case "error", "fatal", "panic":
		f.logger.ErrorContext(ctx, msg, attrs...)
	default:
		f.logger.InfoContext(ctx, msg, attrs...)
	}
}
