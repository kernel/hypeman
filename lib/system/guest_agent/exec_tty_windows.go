//go:build windows

package main

import (
	"context"
	"fmt"
	"log"
	"sync"
	"syscall"

	pty "github.com/aymanbagabas/go-pty"
	pb "github.com/kernel/hypeman/lib/guest"
)

func (s *guestServer) executeTTY(ctx context.Context, stream pb.GuestService_ExecServer, start *pb.ExecStart) error {
	if len(start.Command) == 0 {
		return fmt.Errorf("empty command")
	}

	console, err := pty.New()
	if err != nil {
		return fmt.Errorf("create ConPTY: %w", err)
	}
	defer console.Close()

	cols, rows := int(start.Cols), int(start.Rows)
	if cols == 0 {
		cols = 80
	}
	if rows == 0 {
		rows = 24
	}
	if err := console.Resize(cols, rows); err != nil {
		return fmt.Errorf("resize ConPTY: %w", err)
	}

	cmd := console.CommandContext(ctx, start.Command[0], start.Command[1:]...)
	cmd.Env = s.buildEnv(start.Env, true)
	cmd.Dir = start.Cwd
	token, cleanup, err := executionToken(start.Session)
	if err != nil {
		return err
	}
	defer cleanup()
	if token != 0 {
		cmd.SysProcAttr = &syscall.SysProcAttr{Token: token}
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start ConPTY command: %w", err)
	}

	var sendMu sync.Mutex
	var wg sync.WaitGroup
	go func() {
		for {
			req, err := stream.Recv()
			if err != nil {
				return
			}
			if data := req.GetStdin(); data != nil {
				_, _ = console.Write(data)
			}
			if resize := req.GetResize(); resize != nil {
				_ = console.Resize(int(resize.Cols), int(resize.Rows))
			}
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		buf := make([]byte, 32*1024)
		for {
			n, err := console.Read(buf)
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
	_ = console.Close()
	wg.Wait()
	exitCode := int32(0)
	if cmd.ProcessState != nil {
		exitCode = int32(cmd.ProcessState.ExitCode())
	} else if waitErr != nil {
		exitCode = 124
	}
	log.Printf("[guest-agent] ConPTY command finished with exit code: %d", exitCode)
	return stream.Send(&pb.ExecResponse{Response: &pb.ExecResponse_ExitCode{ExitCode: exitCode}})
}
