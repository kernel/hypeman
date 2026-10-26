package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	pb "github.com/kernel/hypeman/lib/guest"
	"github.com/stretchr/testify/require"
)

// execProcessGone reports whether pid has exited or is a zombie. A zombie is not
// running, but kill(pid, 0) still succeeds for it, so ps reports its state instead.
func execProcessGone(pid int) bool {
	out, err := exec.Command("ps", "-o", "stat=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return true
	}
	state := strings.TrimSpace(string(out))
	return state == "" || strings.HasPrefix(state, "Z")
}

// On a TTY, a timeout that fires while the exec drains output after the shell exited
// must still kill the background child that holds the terminal open.
func TestExecTimeoutDuringDrainKillsBackgroundChild(t *testing.T) {
	if runtime.GOOS == "darwin" {
		// macOS revokes the terminal when the session leader exits, so a background
		// child cannot hold the drain open there. The pipe variant covers the drain.
		t.Skip("terminal drain is revoked on macOS when the shell exits")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	release := make(chan struct{})
	defer close(release)
	pidFile := filepath.Join(t.TempDir(), "child")
	stream := &exitCapturingExecStream{stalledExecStream: stalledExecStream{ctx: ctx, release: release, start: &pb.ExecRequest{Request: &pb.ExecRequest_Start{Start: &pb.ExecStart{
		Tty:            true,
		TimeoutSeconds: 3,
		Command:        []string{"/bin/sh", "-c", `trap '' HUP; sleep 60 & echo $! > "$1"; exit 0`, "sh", pidFile},
	}}}}}

	started := time.Now()
	require.NoError(t, (&guestServer{execLifecycle: true, drainGrace: 5 * time.Second}).Exec(stream))
	require.Less(t, time.Since(started), 7*time.Second, "the timeout must end the exec, not the drain grace")
	require.NotNil(t, stream.exitCode)
	require.EqualValues(t, 124, *stream.exitCode, "a timed-out exec reports 124")

	data, err := os.ReadFile(pidFile)
	require.NoError(t, err, "background child pid was not written")
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	require.NoError(t, err)
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
	require.Eventually(t, func() bool { return execProcessGone(pid) }, 2*time.Second, 50*time.Millisecond,
		"background child %d survived the timeout", pid)
}

// The final status is delivered within the drain bound even when no cancellation
// ever happens and the client never reads.
func TestSendExitCodeBoundedWhenClientStopsReading(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	release := make(chan struct{})
	defer close(release)
	stream := &stalledExecStream{ctx: ctx, release: release}
	sender := &execSender{stream: stream}

	server := &guestServer{drainGrace: 200 * time.Millisecond}
	done := make(chan error, 1)
	go func() { done <- server.sendExitCode(ctx, sender, 0) }()
	select {
	case err := <-done:
		require.Error(t, err, "a stalled client must fail the status send, not succeed")
	case <-time.After(3 * time.Second):
		t.Fatal("sendExitCode blocked on a client that stopped reading")
	}
}

// The same drain on pipes, which every platform keeps open for a background child.
func TestExecTimeoutDuringPipeDrainKillsBackgroundChild(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	release := make(chan struct{})
	defer close(release)
	pidFile := filepath.Join(t.TempDir(), "child")
	stream := &exitCapturingExecStream{stalledExecStream: stalledExecStream{ctx: ctx, release: release, start: &pb.ExecRequest{Request: &pb.ExecRequest_Start{Start: &pb.ExecStart{
		TimeoutSeconds: 3,
		Command:        []string{"/bin/sh", "-c", `sleep 60 & echo $! > "$1"; exit 0`, "sh", pidFile},
	}}}}}

	started := time.Now()
	require.NoError(t, (&guestServer{execLifecycle: true, drainGrace: 5 * time.Second}).Exec(stream))
	require.Less(t, time.Since(started), 7*time.Second, "the timeout must end the exec, not the drain grace")
	require.NotNil(t, stream.exitCode)
	require.EqualValues(t, 124, *stream.exitCode, "a timed-out exec reports 124")

	data, err := os.ReadFile(pidFile)
	require.NoError(t, err, "background child pid was not written")
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	require.NoError(t, err)
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
	require.Eventually(t, func() bool { return execProcessGone(pid) }, 2*time.Second, 50*time.Millisecond,
		"background child %d survived the timeout", pid)
}
