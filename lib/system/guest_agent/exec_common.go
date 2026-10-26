package main

import (
	"context"
	"os"
	"os/exec"

	"github.com/creack/pty"
	pb "github.com/kernel/hypeman/lib/guest"
)

// Both exec paths, the direct path and the macOS lifecycle in exec_lifecycle.go,
// share command setup, PTY start, input handling and exit status. They differ in
// policy, which stays in their own files: the direct path buffers stdout and waits
// without a bound, the lifecycle streams output and bounds its drain, and a timeout
// kills the direct child or the whole process group.

// newExecCmd builds the command both paths run. Only a TTY session gets TERM defaults.
func (s *guestServer) newExecCmd(ctx context.Context, start *pb.ExecStart) *exec.Cmd {
	cmd := exec.CommandContext(ctx, start.Command[0], start.Command[1:]...)
	cmd.Env = s.buildEnv(start.Env, start.Tty)
	if start.Cwd != "" {
		cmd.Dir = start.Cwd
	}
	return cmd
}

// startExecPTY starts cmd on a PTY sized from start, with 24x80 defaults.
func startExecPTY(cmd *exec.Cmd, start *pb.ExecStart) (*os.File, error) {
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
	return pty.StartWithSize(cmd, ws)
}

// pumpExecInput forwards client stdin and resize messages until the stream ends.
// onResize may be nil. onClose, if set, runs when the stream ends.
func pumpExecInput(stream pb.GuestService_ExecServer, onStdin func([]byte), onResize func(*pb.WindowSize), onClose func()) {
	go func() {
		if onClose != nil {
			defer onClose()
		}
		for {
			req, err := stream.Recv()
			if err != nil {
				return
			}
			if data := req.GetStdin(); data != nil {
				onStdin(data)
			}
			if resize := req.GetResize(); resize != nil && onResize != nil {
				onResize(resize)
			}
		}
	}()
}

// execExitCode is the status the macOS lifecycle reports. A deadline always reports
// 124, the GNU timeout convention, even when the command exited during the drain.
// Otherwise it is the process exit status.
func execExitCode(cmd *exec.Cmd, waitErr error, timedOut bool) int32 {
	if timedOut {
		return 124
	}
	return processExitCode(cmd, waitErr)
}

// processExitCode is the status the direct (Linux) path reports: the process exit
// code, which is -1 for a command a timeout killed, and 124 only for a command that
// could not be waited for. The deadline is not consulted, so a command that finished
// in time keeps its code even if sending its output outlasts the deadline.
func processExitCode(cmd *exec.Cmd, waitErr error) int32 {
	if cmd.ProcessState != nil {
		return int32(cmd.ProcessState.ExitCode())
	}
	if waitErr != nil {
		return 124
	}
	return 0
}
