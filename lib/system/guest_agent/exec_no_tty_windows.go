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

func (s *guestServer) executeNoTTY(ctx context.Context, stream pb.GuestService_ExecServer, start *pb.ExecStart) error {
	if len(start.Command) == 0 {
		return fmt.Errorf("empty command")
	}
	if start.Session == pb.ExecSession_EXEC_SESSION_DESKTOP {
		return s.executeDesktopNoTTY(ctx, stream, start)
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
	return sendCapturedCommandResult(stream, stdoutFile, stderrFile, cmd.ProcessState, waitErr)
}

func (s *guestServer) executeDesktopNoTTY(ctx context.Context, stream pb.GuestService_ExecServer, start *pb.ExecStart) error {
	stdinRead, stdinWrite, err := os.Pipe()
	if err != nil {
		return fmt.Errorf("create desktop command stdin: %w", err)
	}
	stdoutFile, err := os.CreateTemp("", "hypeman-exec-stdout-*")
	if err != nil {
		stdinRead.Close()
		stdinWrite.Close()
		return fmt.Errorf("create stdout capture: %w", err)
	}
	defer func() {
		stdoutFile.Close()
		os.Remove(stdoutFile.Name())
	}()
	stderrFile, err := os.CreateTemp("", "hypeman-exec-stderr-*")
	if err != nil {
		stdinRead.Close()
		stdinWrite.Close()
		return fmt.Errorf("create stderr capture: %w", err)
	}
	defer func() {
		stderrFile.Close()
		os.Remove(stderrFile.Name())
	}()

	process, cleanup, err := startDesktopProcess(
		ctx,
		start.Command,
		s.buildEnv(start.Env, false),
		start.Cwd,
		stdinRead,
		stdoutFile,
		stderrFile,
		start.TimeoutSeconds,
	)
	stdinRead.Close()
	if err != nil {
		stdinWrite.Close()
		return err
	}
	defer cleanup()
	go func() {
		defer stdinWrite.Close()
		for {
			req, err := stream.Recv()
			if err != nil {
				return
			}
			if data := req.GetStdin(); data != nil {
				_, _ = stdinWrite.Write(data)
			}
		}
	}()

	processState, waitErr := process.Wait()
	return sendCapturedCommandResult(stream, stdoutFile, stderrFile, processState, waitErr)
}

func sendCapturedCommandResult(
	stream pb.GuestService_ExecServer,
	stdoutFile, stderrFile *os.File,
	processState *os.ProcessState,
	waitErr error,
) error {
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
	if processState != nil {
		exitCode = int32(processState.ExitCode())
	} else if waitErr != nil {
		exitCode = 124
	}
	log.Printf("[guest-agent] command finished with exit code: %d", exitCode)
	return stream.Send(&pb.ExecResponse{Response: &pb.ExecResponse_ExitCode{ExitCode: exitCode}})
}
