package main

import (
	"context"
	"io"
	"strings"
	"sync"
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
