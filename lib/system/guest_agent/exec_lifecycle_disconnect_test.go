package main

import (
	"context"
	"fmt"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	pb "github.com/kernel/hypeman/lib/guest"
	"github.com/stretchr/testify/require"
)

// On the macOS exec path, a client disconnect cancels the command and its descendants.

func TestCancelledExecDoesNotAttemptStart(t *testing.T) {
	for _, tty := range []bool{false, true} {
		t.Run(fmt.Sprintf("tty=%t", tty), func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			stream := &stalledExecStream{ctx: ctx, start: &pb.ExecRequest{Request: &pb.ExecRequest_Start{Start: &pb.ExecStart{Command: []string{filepath.Join(t.TempDir(), "nonexistent-executable")}, Tty: tty}}}}
			err := (&guestServer{execLifecycle: true}).Exec(stream)
			require.ErrorIs(t, err, context.Canceled, "a canceled request must be rejected before trying to start the command")
		})
	}
}

func TestGuestServiceExecCancellation(t *testing.T) {
	client := testGuestClient(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pidPath := filepath.Join(t.TempDir(), "pid")
	stream, err := client.Exec(ctx)
	require.NoError(t, err)
	require.NoError(t, stream.Send(&pb.ExecRequest{Request: &pb.ExecRequest_Start{Start: &pb.ExecStart{
		Command: []string{"/bin/sh", "-c", "printf '%s' \"$$\" > \"$HYPEMAN_PID_FILE\"; exec /bin/sleep 30"},
		Env:     map[string]string{"HYPEMAN_PID_FILE": pidPath},
	}}}))
	require.NoError(t, stream.CloseSend())
	pid := waitForPIDFile(t, pidPath)
	defer syscall.Kill(pid, syscall.SIGKILL)
	cancel()
	require.Eventually(t, func() bool {
		return syscall.Kill(pid, 0) == syscall.ESRCH
	}, 5*time.Second, 10*time.Millisecond, "disconnect must cancel the command, not leave it running")
}

func TestGuestServiceExecCancellationKillsDescendants(t *testing.T) {
	for _, tty := range []bool{false, true} {
		t.Run(map[bool]string{false: "pipes", true: "tty"}[tty], func(t *testing.T) {
			client := testGuestClient(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			pidPath := filepath.Join(t.TempDir(), "pid")
			stream, err := client.Exec(ctx)
			require.NoError(t, err)
			require.NoError(t, stream.Send(&pb.ExecRequest{Request: &pb.ExecRequest_Start{Start: &pb.ExecStart{
				Command: []string{"/bin/sh", "-c", "sleep 30 & printf '%s' \"$!\" > \"$HYPEMAN_PID_FILE\"; wait"},
				Env:     map[string]string{"HYPEMAN_PID_FILE": pidPath},
				Tty:     tty,
			}}}))
			require.NoError(t, stream.CloseSend())
			pid := waitForPIDFile(t, pidPath)
			defer syscall.Kill(pid, syscall.SIGKILL)
			cancel()
			require.Eventually(t, func() bool {
				return processGone(pid)
			}, 5*time.Second, 10*time.Millisecond, "cancellation must reach descendants, not only the shell")
		})
	}
}
