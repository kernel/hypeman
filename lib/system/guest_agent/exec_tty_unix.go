//go:build !windows

package main

import (
	"context"
	"fmt"
	"log"
	"os/exec"
	"sync"

	"github.com/creack/pty"
	pb "github.com/kernel/hypeman/lib/guest"
)

func (s *guestServer) executeTTY(ctx context.Context, stream pb.GuestService_ExecServer, start *pb.ExecStart) error {
	if len(start.Command) == 0 {
		return fmt.Errorf("empty command")
	}

	cmd := exec.CommandContext(ctx, start.Command[0], start.Command[1:]...)
	cmd.Env = s.buildEnv(start.Env, true)
	cmd.Dir = start.Cwd

	ws := &pty.Winsize{Rows: uint16(start.Rows), Cols: uint16(start.Cols)}
	if ws.Rows == 0 {
		ws.Rows = 24
	}
	if ws.Cols == 0 {
		ws.Cols = 80
	}

	ptmx, err := pty.StartWithSize(cmd, ws)
	if err != nil {
		return fmt.Errorf("start pty: %w", err)
	}
	defer ptmx.Close()

	var sendMu sync.Mutex
	var wg sync.WaitGroup
	go func() {
		for {
			req, err := stream.Recv()
			if err != nil {
				return
			}
			if data := req.GetStdin(); data != nil {
				_, _ = ptmx.Write(data)
			}
			if resize := req.GetResize(); resize != nil {
				_ = pty.Setsize(ptmx, &pty.Winsize{Rows: uint16(resize.Rows), Cols: uint16(resize.Cols)})
			}
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		buf := make([]byte, 32*1024)
		for {
			n, err := ptmx.Read(buf)
			if n > 0 {
				sendMu.Lock()
				_ = stream.Send(&pb.ExecResponse{Response: &pb.ExecResponse_Stdout{Stdout: buf[:n]}})
				sendMu.Unlock()
			}
			if err != nil {
				return
			}
		}
	}()

	waitErr := cmd.Wait()
	wg.Wait()
	exitCode := int32(0)
	if cmd.ProcessState != nil {
		exitCode = int32(cmd.ProcessState.ExitCode())
	} else if waitErr != nil {
		exitCode = 124
	}
	log.Printf("[guest-agent] TTY command finished with exit code: %d", exitCode)
	return stream.Send(&pb.ExecResponse{Response: &pb.ExecResponse_ExitCode{ExitCode: exitCode}})
}
