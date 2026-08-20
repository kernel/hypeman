package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"strings"
	"sync"
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

	// Create context with timeout if specified
	ctx := context.Background()
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

// executeNoTTY executes command without TTY
func (s *guestServer) executeNoTTY(ctx context.Context, stream pb.GuestService_ExecServer, start *pb.ExecStart) error {
	// Run command directly - guest-agent is already running in container namespace
	if len(start.Command) == 0 {
		return fmt.Errorf("empty command")
	}

	cmd := exec.CommandContext(ctx, start.Command[0], start.Command[1:]...)
	cleanup, err := configureExecCommand(cmd, start.Session)
	if err != nil {
		return err
	}
	defer cleanup()

	// Set up environment (no TTY defaults for non-TTY mode)
	cmd.Env = s.buildEnv(start.Env, false)

	// Set up working directory
	if start.Cwd != "" {
		cmd.Dir = start.Cwd
	}

	stdin, _ := cmd.StdinPipe()
	stdout, _ := cmd.StdoutPipe()
	stderr, _ := cmd.StderrPipe()

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start command: %w", err)
	}

	// Mutex to protect concurrent stream.Send calls (gRPC streams are not thread-safe)
	var sendMu sync.Mutex

	// Use WaitGroup to ensure all output is read before sending
	var wg sync.WaitGroup
	var stdoutData, stderrData []byte

	// Handle stdin in background
	go func() {
		defer stdin.Close()
		for {
			req, err := stream.Recv()
			if err != nil {
				return
			}
			if data := req.GetStdin(); data != nil {
				stdin.Write(data)
			}
		}
	}()

	// Read all stdout/stderr BEFORE calling Wait() - Wait() closes the pipes!
	wg.Add(1)
	go func() {
		defer wg.Done()
		data, _ := io.ReadAll(stdout)
		stdoutData = data
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		data, _ := io.ReadAll(stderr)
		stderrData = data
	}()

	// Wait for all reads to complete FIRST (before Wait closes pipes)
	wg.Wait()

	// Now safe to call Wait - pipes are fully drained
	waitErr := cmd.Wait()

	// Now stream output in chunks (streaming compatible)
	const chunkSize = 32 * 1024
	for i := 0; i < len(stdoutData); i += chunkSize {
		end := i + chunkSize
		if end > len(stdoutData) {
			end = len(stdoutData)
		}
		sendMu.Lock()
		stream.Send(&pb.ExecResponse{
			Response: &pb.ExecResponse_Stdout{Stdout: stdoutData[i:end]},
		})
		sendMu.Unlock()
	}
	for i := 0; i < len(stderrData); i += chunkSize {
		end := i + chunkSize
		if end > len(stderrData) {
			end = len(stderrData)
		}
		sendMu.Lock()
		stream.Send(&pb.ExecResponse{
			Response: &pb.ExecResponse_Stderr{Stderr: stderrData[i:end]},
		})
		sendMu.Unlock()
	}

	exitCode := int32(0)
	if cmd.ProcessState != nil {
		exitCode = int32(cmd.ProcessState.ExitCode())
	} else if waitErr != nil {
		// If killed by timeout, exit with 124 (GNU timeout convention)
		exitCode = 124
	}

	log.Printf("[guest-agent] command finished with exit code: %d", exitCode)

	// Send exit code
	return stream.Send(&pb.ExecResponse{
		Response: &pb.ExecResponse_ExitCode{ExitCode: exitCode},
	})
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
