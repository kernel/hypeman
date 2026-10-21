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
// executeLifecycleNoTTY streams output as the command writes it. The pipes are read
// here, not by cmd.Wait, which would reap the leader before the output drain.
func (s *guestServer) executeLifecycleNoTTY(ctx context.Context, cancel context.CancelFunc, sender *execSender, cmd *exec.Cmd) error {
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("open command stdin: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("open command stdout: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("open command stderr: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start command: %w", err)
	}
	grp := newProcessGroup(cmd)
	// A descendant that holds the pipes outlives the leader. The group is killed from
	// the context, which is the only killer.
	lifecycleDone := make(chan struct{})
	defer close(lifecycleDone)
	go killGroupOnDone(ctx, grp, lifecycleDone)

	var copies sync.WaitGroup
	copies.Add(2)
	go func() {
		defer copies.Done()
		_, _ = io.Copy(&execStreamWriter{sender: sender, cancel: cancel}, stdout)
	}()
	go func() {
		defer copies.Done()
		_, _ = io.Copy(&execStreamWriter{sender: sender, cancel: cancel, stderr: true}, stderr)
	}()
	outputDone := make(chan struct{})
	go func() {
		copies.Wait()
		close(outputDone)
	}()

	pumpExecInput(sender.stream, func(data []byte) { stdin.Write(data) }, nil, func() { stdin.Close() })

	return s.finishExec(ctx, sender, cmd, grp, outputDone)
}

// execCommand owns setup and pre-start cancellation for both I/O modes.
func (s *guestServer) execCommand(ctx context.Context, start *pb.ExecStart) (*exec.Cmd, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("start command: %w", err)
	}
	cmd := s.newExecCmd(ctx, start)
	// The group watcher kills the group, so it can check whether the leader was reaped.
	cmd.Cancel = func() error { return nil }
	cmd.WaitDelay = 2 * time.Second
	if !start.Tty {
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	}
	return cmd, nil
}

// finishExec owns bounded wait, output drain and final status for both I/O modes.
func (s *guestServer) finishExec(ctx context.Context, sender *execSender, cmd *exec.Cmd, grp *processGroup, outputDone <-chan struct{}) error {
	// The leader exits, or the context kills the group, and the leader stays unreaped
	// until reap below. Its group id stays reserved, so a deadline during the output
	// drain can still kill background children that hold the output open.
	if err := waitLeaderExit(grp.pid); err != nil {
		log.Printf("[guest-agent] wait for command exit: %v", err)
	}
	var drainErr error
	if outputDone != nil {
		drainErr = drainAfterExit(ctx, outputDone, s.drainBound())
	}
	waitErr := grp.reap(cmd)
	if drainErr != nil {
		return drainErr
	}
	if waitErr != nil {
		// exec.CommandContext reports the context error from Wait once the context ends.
		// That is a cancellation, not a failed stream, and execExitCode maps it.
		_, exited := waitErr.(*exec.ExitError)
		cancelled := ctx.Err() != nil && errors.Is(waitErr, ctx.Err())
		if !exited && !cancelled {
			return fmt.Errorf("stream command output: %w", waitErr)
		}
	}
	exitCode := execExitCode(cmd, waitErr, errors.Is(ctx.Err(), context.DeadlineExceeded))

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

// sendExitCode delivers the final status within the drain bound, counted from the
// send itself, so a client that stopped reading cannot hold the handler even when
// nothing is cancelled.
func (s *guestServer) sendExitCode(ctx context.Context, sender *execSender, exitCode int32) error {
	sent := make(chan error, 1)
	go func() {
		sent <- sender.send(&pb.ExecResponse{Response: &pb.ExecResponse_ExitCode{ExitCode: exitCode}})
	}()
	timer := time.NewTimer(s.drainBound())
	defer timer.Stop()
	select {
	case err := <-sent:
		return err
	case <-timer.C:
		if ctx.Err() != nil {
			return fmt.Errorf("command did not finish after cancellation: %w", ctx.Err())
		}
		return fmt.Errorf("exit status not delivered within %s: client stopped reading", s.drainBound())
	}
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
	ptmx, err := startExecPTY(cmd, start)
	if err != nil {
		return fmt.Errorf("start pty: %w", err)
	}
	defer ptmx.Close()
	grp := newProcessGroup(cmd)
	lifecycleDone := make(chan struct{})
	defer close(lifecycleDone)
	go killGroupOnDone(ctx, grp, lifecycleDone)

	outputDone := make(chan struct{})
	output := &execStreamWriter{sender: sender, cancel: cancel}

	pumpExecInput(sender.stream, func(data []byte) { ptmx.Write(data) }, func(resize *pb.WindowSize) {
		pty.Setsize(ptmx, &pty.Winsize{
			Rows: uint16(resize.Rows),
			Cols: uint16(resize.Cols),
		})
	}, nil)

	// Stream output
	go func() {
		defer close(outputDone)
		_, _ = io.Copy(output, ptmx)
	}()

	return s.finishExec(ctx, sender, cmd, grp, outputDone)
}
