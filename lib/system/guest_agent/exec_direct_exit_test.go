package main

import (
	"context"
	"testing"
	"time"

	pb "github.com/kernel/hypeman/lib/guest"
	"github.com/stretchr/testify/require"
)

// slowOutputExecStream delays each output send, so delivering output can outlast an
// exec deadline after the command has already exited. It records the exit code.
type slowOutputExecStream struct {
	stalledExecStream
	delay    time.Duration
	exitCode *int32
}

func (s *slowOutputExecStream) Send(response *pb.ExecResponse) error {
	if _, ok := response.Response.(*pb.ExecResponse_ExitCode); ok {
		value := response.GetExitCode()
		s.exitCode = &value
		return nil
	}
	time.Sleep(s.delay)
	return nil
}

// The direct (Linux) exec path reports the process status. A command that exits in
// time keeps its code even if delivering its output outlasts the deadline, and a
// command a timeout kills reports -1, as before the macOS work.
func TestDirectExecReportsProcessExitCode(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for _, tty := range []bool{false, true} {
		run := func(command string, timeout int32, delay time.Duration) int32 {
			stream := &slowOutputExecStream{delay: delay, stalledExecStream: stalledExecStream{ctx: ctx, start: &pb.ExecRequest{Request: &pb.ExecRequest_Start{Start: &pb.ExecStart{
				Tty: tty, TimeoutSeconds: timeout, Command: []string{"/bin/sh", "-c", command},
			}}}}}
			require.NoError(t, (&guestServer{}).Exec(stream))
			require.NotNil(t, stream.exitCode, "tty=%v: exit status must be reported", tty)
			return *stream.exitCode
		}
		require.EqualValues(t, 3, run("echo done; exit 3", 1, 1500*time.Millisecond), "tty=%v: slow output delivery must not turn an exit into 124", tty)
		require.EqualValues(t, -1, run("exec sleep 30", 1, 0), "tty=%v: a timed-out command reports its signal status", tty)
	}
}
