package main

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"

	pb "github.com/kernel/hypeman/lib/guest"
)

// recordingExecStream is a fake Exec server stream that timestamps stdout chunks.
type recordingExecStream struct {
	grpc.ServerStream
	ctx    context.Context
	mu     sync.Mutex
	stdout []timedChunk
}

type timedChunk struct {
	at   time.Time
	data string
}

func (s *recordingExecStream) Context() context.Context { return s.ctx }
func (s *recordingExecStream) Recv() (*pb.ExecRequest, error) {
	return nil, io.EOF
}

func (s *recordingExecStream) Send(resp *pb.ExecResponse) error {
	if out := resp.GetStdout(); out != nil {
		s.mu.Lock()
		s.stdout = append(s.stdout, timedChunk{at: time.Now(), data: string(out)})
		s.mu.Unlock()
	}
	return nil
}

func TestExecuteNoTTYStreamsOutputBeforeExit(t *testing.T) {
	stream := &recordingExecStream{ctx: t.Context()}
	start := time.Now()
	err := (&guestServer{}).executeNoTTY(t.Context(), stream, &pb.ExecStart{
		Command: []string{"sh", "-c", "echo first; sleep 2; echo second"},
	})
	require.NoError(t, err)
	exitedAt := time.Since(start)

	require.NotEmpty(t, stream.stdout)
	require.True(t, strings.HasPrefix(stream.stdout[0].data, "first"))
	firstAt := stream.stdout[0].at.Sub(start)
	require.Less(t, firstAt, exitedAt-time.Second,
		"first chunk arrived at %s but command exited at %s; output is being buffered", firstAt, exitedAt)

	var all strings.Builder
	for _, c := range stream.stdout {
		all.WriteString(c.data)
	}
	require.Equal(t, "first\nsecond\n", all.String())
}

// funcExecStream is a fake Exec server stream with pluggable Recv and Send.
type funcExecStream struct {
	grpc.ServerStream
	ctx  context.Context
	recv func() (*pb.ExecRequest, error)
	send func(*pb.ExecResponse) error
}

func (s *funcExecStream) Context() context.Context       { return s.ctx }
func (s *funcExecStream) Recv() (*pb.ExecRequest, error) { return s.recv() }
func (s *funcExecStream) Send(r *pb.ExecResponse) error  { return s.send(r) }

// runWithDeadline fails the test if fn has not returned within d.
func runWithDeadline(t *testing.T, d time.Duration, fn func() error) error {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- fn() }()
	select {
	case err := <-done:
		return err
	case <-time.After(d):
		t.Fatalf("exec handler did not return within %s", d)
		return nil
	}
}

// requireReaped asserts the pid written to pidFile no longer exists (not even as a zombie).
func requireReaped(t *testing.T, pidFile string) {
	t.Helper()
	raw, err := os.ReadFile(pidFile)
	require.NoError(t, err)
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	require.NoError(t, err)
	require.ErrorIs(t, syscall.Kill(pid, 0), syscall.ESRCH, "pid %d was not reaped", pid)
}

func TestExecuteNoTTYReapsWhenClientStalls(t *testing.T) {
	prev := execDrainTimeout
	execDrainTimeout = 500 * time.Millisecond
	t.Cleanup(func() { execDrainTimeout = prev })

	// Send blocks like a gRPC stream with an exhausted flow-control window.
	streamCtx, streamCancel := context.WithCancel(t.Context())
	t.Cleanup(streamCancel)
	stream := &funcExecStream{
		ctx:  streamCtx,
		recv: func() (*pb.ExecRequest, error) { <-streamCtx.Done(); return nil, io.EOF },
		send: func(*pb.ExecResponse) error { <-streamCtx.Done(); return streamCtx.Err() },
	}

	pidFile := filepath.Join(t.TempDir(), "pid")
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	err := runWithDeadline(t, 10*time.Second, func() error {
		return (&guestServer{}).executeNoTTY(ctx, stream, &pb.ExecStart{
			Command: []string{"sh", "-c", `echo $$ > "$1"; exec yes`, "sh", pidFile},
		})
	})
	require.ErrorContains(t, err, "not drained")
	requireReaped(t, pidFile)
}

func TestExecuteNoTTYKillsCommandWhenSendFails(t *testing.T) {
	sendErr := errors.New("client gone")
	stream := &funcExecStream{
		ctx:  t.Context(),
		recv: func() (*pb.ExecRequest, error) { return nil, io.EOF },
		send: func(*pb.ExecResponse) error { return sendErr },
	}

	pidFile := filepath.Join(t.TempDir(), "pid")
	err := runWithDeadline(t, 10*time.Second, func() error {
		return (&guestServer{}).executeNoTTY(t.Context(), stream, &pb.ExecStart{
			Command: []string{"sh", "-c", `echo $$ > "$1"; echo started; exec sleep 30`, "sh", pidFile},
		})
	})
	require.ErrorIs(t, err, sendErr)
	requireReaped(t, pidFile)
}

func TestExecKillsCommandWhenClientDisconnects(t *testing.T) {
	streamCtx, disconnect := context.WithCancel(t.Context())
	defer disconnect()
	pidFile := filepath.Join(t.TempDir(), "pid")
	started := false
	stream := &funcExecStream{
		ctx: streamCtx,
		recv: func() (*pb.ExecRequest, error) {
			if !started {
				started = true
				return &pb.ExecRequest{Request: &pb.ExecRequest_Start{Start: &pb.ExecStart{
					Command: []string{"sh", "-c", `echo $$ > "$1"; exec sleep 30`, "sh", pidFile},
				}}}, nil
			}
			<-streamCtx.Done()
			return nil, streamCtx.Err()
		},
		send: func(*pb.ExecResponse) error { return streamCtx.Err() },
	}

	time.AfterFunc(500*time.Millisecond, disconnect)
	_ = runWithDeadline(t, 10*time.Second, func() error {
		return (&guestServer{}).Exec(stream)
	})
	requireReaped(t, pidFile)
}
