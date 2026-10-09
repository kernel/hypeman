package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
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

	if len(start.Command) == 0 {
		start.Command = []string{"/bin/sh"}
	}
	command := start.Command

	log.Printf("[guest-agent] exec: command=%v tty=%v cwd=%s timeout=%d",
		command, start.Tty, start.Cwd, start.TimeoutSeconds)

	// Create context with timeout if specified
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

// executeNoTTY executes command without TTY
func (s *guestServer) executeNoTTY(ctx context.Context, stream pb.GuestService_ExecServer, start *pb.ExecStart) error {
	// Run command directly - guest-agent is already running in container namespace
	if len(start.Command) == 0 {
		return fmt.Errorf("empty command")
	}

	outputCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	// CommandContext must observe both caller cancellation and stream-send errors.
	cmd := exec.CommandContext(outputCtx, start.Command[0], start.Command[1:]...)
	cmd.Env = s.buildEnv(start.Env, false)
	cmd.Dir = start.Cwd
	cmd.WaitDelay = 2 * time.Second
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return killProcessGroup(cmd) }
	var sendMu sync.Mutex
	cmd.Stdout = &execStreamWriter{stream: stream, mu: &sendMu, cancel: cancel}
	cmd.Stderr = &execStreamWriter{stream: stream, mu: &sendMu, cancel: cancel, stderr: true}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("open command stdin: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start command: %w", err)
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

	// cmd.Wait also waits for the stdout/stderr copies, which can block in stream.Send.
	waitDone := make(chan struct{})
	var waitErr error
	go func() {
		waitErr = cmd.Wait()
		close(waitDone)
	}()
	if err := awaitDrain(ctx, waitDone); err != nil {
		return err
	}
	if waitErr != nil {
		if _, exited := waitErr.(*exec.ExitError); !exited {
			return fmt.Errorf("stream command output: %w", waitErr)
		}
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

// execDrainGrace bounds how long a cancelled command may take to finish. A descendant that
// holds the terminal or output, or a client that stopped reading, must not block the handler.
var execDrainGrace = 5 * time.Second

// awaitDrain waits for done. After ctx ends it allows execDrainGrace more, then returns an
// error so the handler returns, ending the RPC and releasing any output send still blocked on it.
func awaitDrain(ctx context.Context, done <-chan struct{}) error {
	select {
	case <-done:
		return nil
	case <-ctx.Done():
	}
	timer := time.NewTimer(execDrainGrace)
	defer timer.Stop()
	select {
	case <-done:
		return nil
	case <-timer.C:
		return fmt.Errorf("command output did not drain after cancellation: %w", ctx.Err())
	}
}

// killProcessGroup kills the command and its descendants. Both exec paths start the command
// as a group leader (Setpgid for no-TTY, Setsid for TTY), so the group id is its pid.
func killProcessGroup(cmd *exec.Cmd) error {
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
	}
	return nil
}

type execStreamWriter struct {
	stream pb.GuestService_ExecServer
	mu     *sync.Mutex
	cancel context.CancelFunc
	stderr bool
}

func (w *execStreamWriter) Write(data []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	written := 0
	for len(data) > 0 {
		n := min(len(data), 32*1024)
		response := &pb.ExecResponse{Response: &pb.ExecResponse_Stdout{Stdout: data[:n]}}
		if w.stderr {
			response.Response = &pb.ExecResponse_Stderr{Stderr: data[:n]}
		}
		if err := w.stream.Send(response); err != nil {
			w.cancel()
			return written, err
		}
		written += n
		data = data[n:]
	}
	return written, nil
}

// executeTTY executes command with TTY
func (s *guestServer) executeTTY(ctx context.Context, stream pb.GuestService_ExecServer, start *pb.ExecStart) error {
	// Run command directly with PTY - guest-agent is already running in container namespace
	// This ensures PTY and shell are in the same namespace, fixing Ctrl+C signal handling
	if len(start.Command) == 0 {
		return fmt.Errorf("empty command")
	}

	cmd := exec.CommandContext(ctx, start.Command[0], start.Command[1:]...)
	cmd.Cancel = func() error { return killProcessGroup(cmd) }

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

	waitDone := make(chan struct{})
	var waitErr error
	go func() {
		waitErr = cmd.Wait()
		close(waitDone)
	}()
	if err := awaitDrain(ctx, waitDone); err != nil {
		return err
	}

	// Wait for all output to be sent
	outputDone := make(chan struct{})
	go func() {
		wg.Wait()
		close(outputDone)
	}()
	if err := awaitDrain(ctx, outputDone); err != nil {
		return err
	}

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
