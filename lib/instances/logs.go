package instances

import (
	"bufio"
	"context"
	"fmt"
	"os/exec"
	"strconv"

	"github.com/onkernel/hypeman/lib/logger"
)

// StreamInstanceLogs streams instance console logs
// Returns last N lines, then continues following if follow=true
func (m *manager) streamInstanceLogs(ctx context.Context, id string, tail int, follow bool) (<-chan string, error) {
	log := logger.FromContext(ctx)
	log.DebugContext(ctx, "starting log stream", "id", id, "tail", tail, "follow", follow)

	if _, err := m.loadMetadata(id); err != nil {
		return nil, err
	}

	logPath := m.paths.InstanceConsoleLog(id)

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
				log.DebugContext(ctx, "log stream cancelled", "id", id)
				return
			case out <- scanner.Text():
			}
		}

		if err := scanner.Err(); err != nil {
			log.ErrorContext(ctx, "scanner error", "id", id, "error", err)
		}

		// Wait for tail to exit (important for non-follow mode)
		cmd.Wait()
	}()

	return out, nil
}
