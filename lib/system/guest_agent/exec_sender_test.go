package main

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	pb "github.com/kernel/hypeman/lib/guest"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
)

// overlapDetectingStream fails the test if two goroutines are inside Send at once.
type overlapDetectingStream struct {
	grpc.ServerStream
	active  atomic.Int32
	overlap atomic.Bool
	sent    atomic.Int32
}

func (s *overlapDetectingStream) Send(*pb.ExecResponse) error {
	if s.active.Add(1) > 1 {
		s.overlap.Store(true)
	}
	time.Sleep(time.Millisecond)
	s.active.Add(-1)
	s.sent.Add(1)
	return nil
}

func (s *overlapDetectingStream) Recv() (*pb.ExecRequest, error) { select {} }

func TestExecSenderSerializesAndStopsAfterClose(t *testing.T) {
	stream := &overlapDetectingStream{}
	sender := &execSender{stream: stream}
	output := &execStreamWriter{sender: sender, cancel: func() {}}

	var wg sync.WaitGroup
	for range 8 {
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, _ = output.Write([]byte("out"))
		}()
		go func() {
			defer wg.Done()
			_ = sender.send(&pb.ExecResponse{Response: &pb.ExecResponse_ExitCode{ExitCode: 0}})
		}()
	}
	wg.Wait()
	require.False(t, stream.overlap.Load(), "output and exit status must never send concurrently")
	require.EqualValues(t, 16, stream.sent.Load())

	sender.close()
	_, err := output.Write([]byte("late"))
	require.ErrorIs(t, err, errExecStreamClosed)
	require.EqualValues(t, 16, stream.sent.Load(), "nothing is sent after the handler returns")
}
