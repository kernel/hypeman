//go:build !windows

package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"os/exec"
	"sync"

	pb "github.com/kernel/hypeman/lib/guest"
)

func (s *guestServer) executeNoTTY(ctx context.Context, stream pb.GuestService_ExecServer, start *pb.ExecStart) error {
	if len(start.Command) == 0 {
		return fmt.Errorf("empty command")
	}

	cmd := exec.CommandContext(ctx, start.Command[0], start.Command[1:]...)
	cleanup, err := configureExecCommand(cmd, start.Session)
	if err != nil {
		return err
	}
	defer cleanup()
	cmd.Env = s.buildEnv(start.Env, false)
	if start.Cwd != "" {
		cmd.Dir = start.Cwd
	}

	stdin, _ := cmd.StdinPipe()
	stdout, _ := cmd.StdoutPipe()
	stderr, _ := cmd.StderrPipe()
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start command: %w", err)
	}

	var sendMu sync.Mutex
	var wg sync.WaitGroup
	var stdoutData, stderrData []byte
	go func() {
		defer stdin.Close()
		for {
			req, err := stream.Recv()
			if err != nil {
				return
			}
			if data := req.GetStdin(); data != nil {
				_, _ = stdin.Write(data)
			}
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		stdoutData, _ = io.ReadAll(stdout)
	}()
	wg.Add(1)
	go func() {
		defer wg.Done()
		stderrData, _ = io.ReadAll(stderr)
	}()
	wg.Wait()
	waitErr := cmd.Wait()

	const chunkSize = 32 * 1024
	for i := 0; i < len(stdoutData); i += chunkSize {
		end := min(i+chunkSize, len(stdoutData))
		sendMu.Lock()
		_ = stream.Send(&pb.ExecResponse{Response: &pb.ExecResponse_Stdout{Stdout: stdoutData[i:end]}})
		sendMu.Unlock()
	}
	for i := 0; i < len(stderrData); i += chunkSize {
		end := min(i+chunkSize, len(stderrData))
		sendMu.Lock()
		_ = stream.Send(&pb.ExecResponse{Response: &pb.ExecResponse_Stderr{Stderr: stderrData[i:end]}})
		sendMu.Unlock()
	}

	exitCode := int32(0)
	if cmd.ProcessState != nil {
		exitCode = int32(cmd.ProcessState.ExitCode())
	} else if waitErr != nil {
		exitCode = 124
	}
	log.Printf("[guest-agent] command finished with exit code: %d", exitCode)
	return stream.Send(&pb.ExecResponse{Response: &pb.ExecResponse_ExitCode{ExitCode: exitCode}})
}
