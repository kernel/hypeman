package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os/exec"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/creack/pty"
	pb "github.com/kernel/hypeman/lib/guest"
)

// execWithLifecycle runs a command for a macOS guest. A client disconnect cancels
// it, cancellation kills its whole process group, and output drains are bounded,
// so an abandoned or stuck command cannot hold the handler.
func (s *guestServer) execWithLifecycle(stream pb.GuestService_ExecServer, start *pb.ExecStart) error {
	if len(start.Command) == 0 {
		start.Command = []string{"/bin/sh"}
	}
	log.Printf("[guest-agent] exec: command=%v tty=%v cwd=%s timeout=%d",
		start.Command, start.Tty, start.Cwd, start.TimeoutSeconds)

	ctx := stream.Context()
	if start.TimeoutSeconds > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(start.TimeoutSeconds)*time.Second)
		defer cancel()
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	cmd, err := s.execCommand(ctx, start)
	if err != nil {
		return err
	}
	sender := &execSender{stream: stream}
	defer sender.close()
	if start.Tty {
		return s.executeLifecycleTTY(ctx, cancel, sender, start, cmd)
	}
	return s.executeLifecycleNoTTY(ctx, cancel, sender, cmd)
}

// errExecStreamClosed is returned for output written after the handler returned.
var errExecStreamClosed = errors.New("exec stream closed")

// execSender serializes sends on one exec stream. A gRPC stream allows one sender
// at a time, and nothing may send once the handler has returned, even though a
// bounded drain can leave output writers running.
type execSender struct {
	stream pb.GuestService_ExecServer
	mu     sync.Mutex
	closed atomic.Bool
}

func (s *execSender) send(response *pb.ExecResponse) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed.Load() {
		return errExecStreamClosed
	}
	return s.stream.Send(response)
}

// close refuses later sends. It does not wait for a send already in progress.
func (s *execSender) close() { s.closed.Store(true) }

// executeLifecycleNoTTY streams output as the command writes it.
func (s *guestServer) executeLifecycleNoTTY(ctx context.Context, cancel context.CancelFunc, sender *execSender, cmd *exec.Cmd) error {
	cmd.Stdout = &execStreamWriter{sender: sender, cancel: cancel}
	cmd.Stderr = &execStreamWriter{sender: sender, cancel: cancel, stderr: true}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("open command stdin: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start command: %w", err)
	}
	// Cancel is not called once the direct child has exited, so a descendant that
	// still holds the pipes would outlive the timeout. Kill the group from the context.
	finished := make(chan struct{})
	defer close(finished)
	go killGroupOnDone(ctx, cmd, finished)

	// Handle stdin in background
	go func() {
		defer stdin.Close()
		for {
			req, err := sender.stream.Recv()
			if err != nil {
				return
			}
			if data := req.GetStdin(); data != nil {
				stdin.Write(data)
			}
		}
	}()

	return s.finishExec(ctx, sender, cmd, nil)
}

// execCommand owns setup and pre-start cancellation for both I/O modes.
func (s *guestServer) execCommand(ctx context.Context, start *pb.ExecStart) (*exec.Cmd, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("start command: %w", err)
	}
	cmd := exec.CommandContext(ctx, start.Command[0], start.Command[1:]...)
	cmd.Cancel = func() error { return killProcessGroup(cmd) }
	cmd.WaitDelay = 2 * time.Second
	cmd.Env = s.buildEnv(start.Env, start.Tty)
	cmd.Dir = start.Cwd
	if !start.Tty {
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	}
	return cmd, nil
}

// finishExec owns bounded wait, output drain and final status for both I/O modes.
func (s *guestServer) finishExec(ctx context.Context, sender *execSender, cmd *exec.Cmd, outputDone <-chan struct{}) error {
	// Non-TTY cmd.Wait also waits for copies, which can block in stream.Send.
	var waitErr error
	if err := s.runBounded(ctx, func() { waitErr = cmd.Wait() }); err != nil {
		return err
	}
	// ErrWaitDelay means the command exited but a descendant still held its output.
	// The status is already known, so it is reported as a normal exit.
	if waitErr != nil {
		_, exited := waitErr.(*exec.ExitError)
		if !exited && !errors.Is(waitErr, exec.ErrWaitDelay) {
			return fmt.Errorf("stream command output: %w", waitErr)
		}
	}

	if outputDone != nil {
		if err := drainAfterExit(ctx, outputDone, s.drainBound()); err != nil {
			return err
		}
	}
	exitCode := exitCodeOf(ctx, cmd, waitErr)

	log.Printf("[guest-agent] command finished with exit code: %d", exitCode)

	return s.sendExitCode(ctx, sender, exitCode)
}

// defaultDrainGrace bounds how long a cancelled command may take to finish. A descendant that
// holds the terminal or output, or a client that stopped reading, must not block the handler.
const defaultDrainGrace = 5 * time.Second

func (s *guestServer) drainBound() time.Duration {
	if s.drainGrace > 0 {
		return s.drainGrace
	}
	return defaultDrainGrace
}

// drainAfterExit runs after the command status is known. A background child can
// keep a terminal open forever, so a normal exit gets only a bounded output drain
// and still reports its status. Cancellation keeps the error-returning drain.
func drainAfterExit(ctx context.Context, done <-chan struct{}, grace time.Duration) error {
	if ctx.Err() != nil {
		return awaitBounded(ctx, done, grace)
	}
	timer := time.NewTimer(grace)
	defer timer.Stop()
	select {
	case <-done:
	case <-timer.C:
		log.Printf("[guest-agent] output still open after command exit; reporting status")
	}
	return nil
}

// awaitBounded waits for done. After ctx ends it allows grace more, then returns an
// error so the handler returns, ending the RPC and releasing any send still blocked on it.
func awaitBounded(ctx context.Context, done <-chan struct{}, grace time.Duration) error {
	select {
	case <-done:
		return nil
	case <-ctx.Done():
	}
	timer := time.NewTimer(grace)
	defer timer.Stop()
	select {
	case <-done:
		return nil
	case <-timer.C:
		return fmt.Errorf("command did not finish after cancellation: %w", ctx.Err())
	}
}

// killGroupOnDone kills cmd's process group once ctx ends, unless finished closes first.
func killGroupOnDone(ctx context.Context, cmd *exec.Cmd, finished <-chan struct{}) {
	select {
	case <-ctx.Done():
		_ = killProcessGroup(cmd)
	case <-finished:
	}
}

// sendExitCode delivers the final status under the same bound as output, so a client
// that stopped reading cannot hold the handler past cancellation.
func (s *guestServer) sendExitCode(ctx context.Context, sender *execSender, exitCode int32) error {
	var err error
	if boundErr := s.runBounded(ctx, func() {
		err = sender.send(&pb.ExecResponse{
			Response: &pb.ExecResponse_ExitCode{ExitCode: exitCode},
		})
	}); boundErr != nil {
		return boundErr
	}
	return err
}

// runBounded runs fn on its own goroutine and waits for it under the drain bound.
// If the bound expires, fn keeps running and must not rely on the handler still waiting.
func (s *guestServer) runBounded(ctx context.Context, fn func()) error {
	done := make(chan struct{})
	go func() {
		fn()
		close(done)
	}()
	return awaitBounded(ctx, done, s.drainBound())
}

// exitCodeOf reports how a command ended. A command that hit its deadline exits 124
// (GNU timeout convention) even though the group kill leaves a process state behind.
func exitCodeOf(ctx context.Context, cmd *exec.Cmd, waitErr error) int32 {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return 124
	}
	if cmd.ProcessState != nil {
		return int32(cmd.ProcessState.ExitCode())
	}
	if waitErr != nil {
		return 124
	}
	return 0
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
	sender *execSender
	cancel context.CancelFunc
	stderr bool
}

func (w *execStreamWriter) Write(data []byte) (int, error) {
	written := 0
	for len(data) > 0 {
		n := min(len(data), 32*1024)
		var response *pb.ExecResponse
		if w.stderr {
			response = &pb.ExecResponse{Response: &pb.ExecResponse_Stderr{Stderr: data[:n]}}
		} else {
			response = &pb.ExecResponse{Response: &pb.ExecResponse_Stdout{Stdout: data[:n]}}
		}
		if err := w.sender.send(response); err != nil {
			w.cancel()
			return written, err
		}
		written += n
		data = data[n:]
	}
	return written, nil
}

// executeLifecycleTTY runs the command on a PTY.
func (s *guestServer) executeLifecycleTTY(ctx context.Context, cancel context.CancelFunc, sender *execSender, start *pb.ExecStart, cmd *exec.Cmd) error {
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
	finished := make(chan struct{})
	defer close(finished)
	go killGroupOnDone(ctx, cmd, finished)

	outputDone := make(chan struct{})
	output := &execStreamWriter{sender: sender, cancel: cancel}

	// Handle stdin and resize in background
	go func() {
		for {
			req, err := sender.stream.Recv()
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
	go func() {
		defer close(outputDone)
		_, _ = io.Copy(output, ptmx)
	}()

	return s.finishExec(ctx, sender, cmd, outputDone)
}
