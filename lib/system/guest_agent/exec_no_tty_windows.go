//go:build windows

package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"time"

	pb "github.com/kernel/hypeman/lib/guest"
)

func (s *guestServer) executeSystemNoTTY(ctx context.Context, stream pb.GuestService_ExecServer, start *pb.ExecStart) error {
	if len(start.Command) == 0 {
		return fmt.Errorf("empty command")
	}

	cmd := execCommand(ctx, start.Command[0], start.Command[1:]...)
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
	stdoutFile, err := os.CreateTemp("", "hypeman-exec-stdout-*")
	if err != nil {
		return fmt.Errorf("create stdout capture: %w", err)
	}
	defer func() {
		stdoutFile.Close()
		os.Remove(stdoutFile.Name())
	}()
	stderrFile, err := os.CreateTemp("", "hypeman-exec-stderr-*")
	if err != nil {
		return fmt.Errorf("create stderr capture: %w", err)
	}
	defer func() {
		stderrFile.Close()
		os.Remove(stderrFile.Name())
	}()
	cmd.Stdout = stdoutFile
	cmd.Stderr = stderrFile

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start command: %w", err)
	}
	jobCleanup, err := attachProcessJob(ctx, cmd.Process, time.Duration(start.TimeoutSeconds)*time.Second)
	if err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return fmt.Errorf("attach process job: %w", err)
	}
	defer jobCleanup()

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

	waitErr := cmd.Wait()
	if _, err := stdoutFile.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("rewind stdout capture: %w", err)
	}
	stdout, err := io.ReadAll(stdoutFile)
	if err != nil {
		return fmt.Errorf("read stdout capture: %w", err)
	}
	if _, err := stderrFile.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("rewind stderr capture: %w", err)
	}
	stderr, err := io.ReadAll(stderrFile)
	if err != nil {
		return fmt.Errorf("read stderr capture: %w", err)
	}

	const chunkSize = 32 * 1024
	for i := 0; i < len(stdout); i += chunkSize {
		end := min(i+chunkSize, len(stdout))
		_ = stream.Send(&pb.ExecResponse{Response: &pb.ExecResponse_Stdout{Stdout: stdout[i:end]}})
	}
	for i := 0; i < len(stderr); i += chunkSize {
		end := min(i+chunkSize, len(stderr))
		_ = stream.Send(&pb.ExecResponse{Response: &pb.ExecResponse_Stderr{Stderr: stderr[i:end]}})
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
