package instances

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"

	"github.com/kernel/hypeman/lib/logger"
)

// LogSource represents a log source type
type LogSource string

const (
	// LogSourceApp is the guest application log (serial console)
	LogSourceApp LogSource = "app"
	// LogSourceVMM is the Cloud Hypervisor VMM log
	LogSourceVMM LogSource = "vmm"
	// LogSourceHypeman is the hypeman operations log
	LogSourceHypeman LogSource = "hypeman"
)

// ErrTailNotFound is returned when the tail command is not available
var ErrTailNotFound = fmt.Errorf("tail command not found: required for log streaming")

// ErrLogNotFound is returned when the requested log file doesn't exist
var ErrLogNotFound = fmt.Errorf("log file not found")

// streamInstanceLogs streams instance logs from the specified source
// Returns last N lines, then continues following if follow=true
func (m *manager) streamInstanceLogs(ctx context.Context, id string, tail int, follow bool, source LogSource) (<-chan string, error) {
	log := logger.FromContext(ctx)
	log.DebugContext(ctx, "starting log stream", "instance_id", id, "tail", tail, "follow", follow, "source", source)

	// Verify tail command is available
	if _, err := exec.LookPath("tail"); err != nil {
		return nil, ErrTailNotFound
	}

	if _, err := m.loadMetadata(id); err != nil {
		return nil, err
	}

	// Determine log path based on source
	var logPath string
	switch source {
	case LogSourceApp:
		logPath = m.paths.InstanceAppLog(id)
	case LogSourceVMM:
		logPath = m.paths.InstanceVMMLog(id)
	case LogSourceHypeman:
		logPath = m.paths.InstanceHypemanLog(id)
	default:
		// Default to app log for backwards compatibility
		logPath = m.paths.InstanceAppLog(id)
	}

	// Check if log file exists before starting tail
	if _, err := os.Stat(logPath); os.IsNotExist(err) {
		return nil, ErrLogNotFound
	}

	// Build tail command
	args := []string{"-n", strconv.Itoa(tail)}
	if follow {
		args = append(args, "-f")
	}
	args = append(args, logPath)

	cmd := exec.CommandContext(ctx, "tail", args...)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("create stdout pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start tail: %w", err)
	}

	out := make(chan string, 100)

	go func() {
		defer close(out)
		defer cmd.Process.Kill()

		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			select {
			case <-ctx.Done():
				log.DebugContext(ctx, "log stream cancelled", "instance_id", id)
				return
			case out <- scanner.Text():
			}
		}

		if err := scanner.Err(); err != nil {
			log.ErrorContext(ctx, "scanner error", "instance_id", id, "error", err)
		}

		// Wait for tail to exit (important for non-follow mode)
		cmd.Wait()
	}()

	return out, nil
}

// rotateLogIfNeeded performs copytruncate rotation if file exceeds maxBytes
// Keeps up to maxFiles old backups (.1, .2, etc.)
func rotateLogIfNeeded(path string, maxBytes int64, maxFiles int) error {
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // Nothing to rotate
		}
		return fmt.Errorf("stat log file: %w", err)
	}

	if info.Size() < maxBytes {
		return nil // Under limit, nothing to do
	}

	// Shift old backups (.1 -> .2, .2 -> .3, etc.)
	for i := maxFiles; i >= 1; i-- {
		oldPath := fmt.Sprintf("%s.%d", path, i)
		newPath := fmt.Sprintf("%s.%d", path, i+1)

		if i == maxFiles {
			// Delete the oldest backup
			os.Remove(oldPath)
		} else {
			// Shift to next number
			os.Rename(oldPath, newPath)
		}
	}

	// Copy current log to .1
	src, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open log for rotation: %w", err)
	}

	dst, err := os.Create(path + ".1")
	if err != nil {
		src.Close()
		return fmt.Errorf("create backup: %w", err)
	}

	_, err = io.Copy(dst, src)
	src.Close()
	dst.Close()
	if err != nil {
		return fmt.Errorf("copy to backup: %w", err)
	}

	// Truncate original (keeps file descriptor valid for writers)
	if err := os.Truncate(path, 0); err != nil {
		return fmt.Errorf("truncate log: %w", err)
	}

	return nil
}
