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

	"github.com/creack/pty"
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

	command := start.Command
	if len(command) == 0 {
		command = []string{"/bin/sh"}
	}

	log.Printf("[guest-agent] exec: command=%v tty=%v cwd=%s timeout=%d",
		command, start.Tty, start.Cwd, start.TimeoutSeconds)

	// Tie the command to the stream so a client disconnect kills it.
	ctx := stream.Context()
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

// execDrainTimeout bounds how long output may keep flowing after the command exits.
var execDrainTimeout = 5 * time.Second

// executeNoTTY executes command without TTY
func (s *guestServer) executeNoTTY(ctx context.Context, stream pb.GuestService_ExecServer, start *pb.ExecStart) error {
	// Run command directly - guest-agent is already running in container namespace
	if len(start.Command) == 0 {
		return fmt.Errorf("empty command")
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	cmd := exec.CommandContext(ctx, start.Command[0], start.Command[1:]...)

	// Set up environment (no TTY defaults for non-TTY mode)
	cmd.Env = s.buildEnv(start.Env, false)

	// Set up working directory
	if start.Cwd != "" {
		cmd.Dir = start.Cwd
	}

	stdin, _ := cmd.StdinPipe()

	// Own the output pipes so cmd.Wait can reap the process without closing them mid-read.
	stdoutR, stdoutW, err := os.Pipe()
	if err != nil {
		return fmt.Errorf("stdout pipe: %w", err)
	}
	defer stdoutR.Close()
	stderrR, stderrW, err := os.Pipe()
	if err != nil {
		stdoutW.Close()
		return fmt.Errorf("stderr pipe: %w", err)
	}
	defer stderrR.Close()
	cmd.Stdout = stdoutW
	cmd.Stderr = stderrW

	err = cmd.Start()
	stdoutW.Close()
	stderrW.Close()
	if err != nil {
		return fmt.Errorf("start command: %w", err)
	}

	// Mutex to protect concurrent stream.Send calls (gRPC streams are not thread-safe)
	var sendMu sync.Mutex
	var sendErr error

	// Stream output as it is produced so long-lived commands (relays, tails)
	// deliver bytes before exit.
	var wg sync.WaitGroup
	pump := func(r io.Reader, wrap func([]byte) *pb.ExecResponse) {
		defer wg.Done()
		buf := make([]byte, 32*1024)
		for {
			n, err := r.Read(buf)
			if n > 0 {
				sendMu.Lock()
				if sendErr == nil {
					if sendErr = stream.Send(wrap(append([]byte(nil), buf[:n]...))); sendErr != nil {
						// Client is gone: kill the command, keep draining so it can't block on a full pipe.
						cancel()
					}
				}
				sendMu.Unlock()
			}
			if err != nil {
				return
			}
		}
	}

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

	wg.Add(2)
	go pump(stdoutR, func(b []byte) *pb.ExecResponse {
		return &pb.ExecResponse{Response: &pb.ExecResponse_Stdout{Stdout: b}}
	})
	go pump(stderrR, func(b []byte) *pb.ExecResponse {
		return &pb.ExecResponse{Response: &pb.ExecResponse_Stderr{Stderr: b}}
	})
	pumpsDone := make(chan struct{})
	go func() {
		wg.Wait()
		close(pumpsDone)
	}()

	// Reap first: a stalled client backpressures the command but never leaks it.
	waitErr := cmd.Wait()

	select {
	case <-pumpsDone:
	case <-time.After(execDrainTimeout):
		// Returning cancels the stream, which unblocks a Send stuck on flow control.
		log.Printf("[guest-agent] exec output not drained %s after exit; abandoning stream", execDrainTimeout)
		return fmt.Errorf("exec output not drained within %s of command exit", execDrainTimeout)
	}
	if sendErr != nil {
		return fmt.Errorf("send output: %w", sendErr)
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

// executeTTY executes command with TTY
func (s *guestServer) executeTTY(ctx context.Context, stream pb.GuestService_ExecServer, start *pb.ExecStart) error {
	// Run command directly with PTY - guest-agent is already running in container namespace
	// This ensures PTY and shell are in the same namespace, fixing Ctrl+C signal handling
	if len(start.Command) == 0 {
		return fmt.Errorf("empty command")
	}

	cmd := exec.CommandContext(ctx, start.Command[0], start.Command[1:]...)

	// Set up environment (TTY mode adds TERM default)
	cmd.Env = s.buildEnv(start.Env, true)

	// Set up working directory
	if start.Cwd != "" {
		cmd.Dir = start.Cwd
	}

	// Set up initial window size (use defaults if not specified)
	ws := &pty.Winsize{
		Rows: uint16(start.Rows),
		Cols: uint16(start.Cols),
	}
	if ws.Rows == 0 {
		ws.Rows = 24
	}
	if ws.Cols == 0 {
		ws.Cols = 80
	}

	// Start with PTY and initial window size
	ptmx, err := pty.StartWithSize(cmd, ws)
	if err != nil {
		return fmt.Errorf("start pty: %w", err)
	}
	defer ptmx.Close()

	// Mutex to protect concurrent stream.Send calls (gRPC streams are not thread-safe)
	var sendMu sync.Mutex

	// Use WaitGroup to ensure all output is sent before exit code
	var wg sync.WaitGroup

	// Handle stdin and resize in background
	go func() {
		for {
			req, err := stream.Recv()
			if err != nil {
				return
			}

			if data := req.GetStdin(); data != nil {
				ptmx.Write(data)
			}

			// Handle window resize
			if resize := req.GetResize(); resize != nil {
				pty.Setsize(ptmx, &pty.Winsize{
					Rows: uint16(resize.Rows),
					Cols: uint16(resize.Cols),
				})
			}
		}
	}()

	// Stream output
	wg.Add(1)
	go func() {
		defer wg.Done()
		buf := make([]byte, 32*1024)
		for {
			n, err := ptmx.Read(buf)
			if n > 0 {
				sendMu.Lock()
				stream.Send(&pb.ExecResponse{
					Response: &pb.ExecResponse_Stdout{Stdout: buf[:n]},
				})
				sendMu.Unlock()
			}
			if err != nil {
				return
			}
		}
	}()

	// Wait for command or context cancellation
	waitErr := cmd.Wait()

	// Wait for all output to be sent
	wg.Wait()

	exitCode := int32(0)
	if cmd.ProcessState != nil {
		exitCode = int32(cmd.ProcessState.ExitCode())
	} else if waitErr != nil {
		// If killed by timeout, exit with 124 (GNU timeout convention)
		exitCode = 124
	}

	log.Printf("[guest-agent] TTY command finished with exit code: %d", exitCode)

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
