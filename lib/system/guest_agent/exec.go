package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	pb "github.com/kernel/hypeman/lib/guest"
)

// Exec handles command execution with bidirectional streaming
func (s *guestServer) Exec(stream pb.GuestService_ExecServer) error {
	log.Printf("[guest-agent] new exec stream")

	// Receive start request
	req, err := stream.Recv()
	if err != nil {
		return fmt.Errorf("receive start request: %w", err)
	}

	start := req.GetStart()
	if start == nil {
		return fmt.Errorf("first message must be ExecStart")
	}

	if len(start.Command) == 0 {
		start.Command = defaultCommand()
	}

	log.Printf("[guest-agent] exec: command=%v tty=%v cwd=%s timeout=%d",
		start.Command, start.Tty, start.Cwd, start.TimeoutSeconds)

	// Windows ties process lifetime to the RPC stream; Unix keeps the existing behavior.
	ctx := execContext(stream.Context())
	if start.TimeoutSeconds > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(start.TimeoutSeconds)*time.Second)
		defer cancel()
	}

	if start.Tty {
		return s.executeTTY(ctx, stream, start)
	}
	return s.executeNoTTY(ctx, stream, start)
}

// buildEnv constructs environment variables by merging provided env with defaults.
// When tty is true, adds sensible defaults for interactive terminal sessions.
// User-provided env vars override both base environment and defaults.
func (s *guestServer) buildEnv(envMap map[string]string, tty bool) []string {
	// Build map of keys to override (user-provided + TTY defaults)
	overrides := make(map[string]string)

	// Add defaults for TTY sessions
	if tty {
		overrides["TERM"] = "xterm-256color"
		overrides["LANG"] = "C.UTF-8"
		overrides["LC_ALL"] = "C.UTF-8"
		overrides["COLORTERM"] = "truecolor"
	}

	// User-provided env vars override defaults
	for k, v := range envMap {
		overrides[k] = v
	}

	// Start with current environment, filtering out keys we'll override
	var env []string
	for _, e := range os.Environ() {
		parts := strings.SplitN(e, "=", 2)
		if len(parts) == 2 {
			if _, override := overrides[parts[0]]; override {
				continue // Skip - we'll add our value
			}
		}
		env = append(env, e)
	}

	// Add overrides
	for k, v := range overrides {
		env = append(env, fmt.Sprintf("%s=%s", k, v))
	}

	return env
}
