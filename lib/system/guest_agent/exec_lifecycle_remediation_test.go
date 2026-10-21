//go:build linux

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

// execProcessGone reports whether pid has exited or is a zombie. A zombie is not
// running, but kill(pid, 0) still succeeds for it, so /proc is the check.
func execProcessGone(pid int) bool {
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return true
	}
	// The state is the field after the parenthesised command name.
	fields := strings.Fields(string(data[strings.LastIndexByte(string(data), ')')+1:]))
	return len(fields) > 0 && fields[0] == "Z"
}

// On a TTY, a timeout that fires while the exec drains output after the shell exited
// must still kill the background child that holds the terminal open.
func TestExecTimeoutDuringDrainKillsBackgroundChild(t *testing.T) {
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
