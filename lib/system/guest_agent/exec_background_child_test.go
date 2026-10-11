package main

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	pb "github.com/kernel/hypeman/lib/guest"
	"github.com/stretchr/testify/require"
)

type exitCapturingExecStream struct {
	stalledExecStream
	exitCode *int32
}

func (s *exitCapturingExecStream) Send(response *pb.ExecResponse) error {
	if _, ok := response.Response.(*pb.ExecResponse_ExitCode); ok {
		value := response.GetExitCode()
		s.exitCode = &value
	}
	return nil
}

// Regression guard: a clean TTY exit with a background child still reports status
// and returns within the bounded drain. This did not reproduce the reviewer's hang
// on macOS (the pty hangs up when the shell exits), so it does not prove that claim.
func TestTTYExecExitStatusWithBackgroundChildIsBounded(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	release := make(chan struct{})
	defer close(release)
	pidFile := filepath.Join(t.TempDir(), "child")
	stream := &exitCapturingExecStream{stalledExecStream: stalledExecStream{ctx: ctx, release: release, start: &pb.ExecRequest{Request: &pb.ExecRequest_Start{Start: &pb.ExecStart{Tty: true, Command: []string{"/bin/sh", "-c", `trap '' HUP; sleep 600 & echo $! > "$1"`, "sh", pidFile}}}}}}
	t.Cleanup(func() {
		if data, err := os.ReadFile(pidFile); err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(data))); err == nil {
				_ = syscall.Kill(pid, syscall.SIGKILL)
			}
		}
	})
	started := time.Now()
	require.NoError(t, (&guestServer{execLifecycle: true, drainGrace: 300 * time.Millisecond}).Exec(stream))
	require.Less(t, time.Since(started), 10*time.Second, "exit must not wait for the background child")
	require.NotNil(t, stream.exitCode, "exit status must be reported")
	require.EqualValues(t, 0, *stream.exitCode)
}
