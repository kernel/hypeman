//go:build windows

package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"sort"
	"strings"
	"time"
	"unicode/utf16"
	"unsafe"

	pb "github.com/kernel/hypeman/lib/guest"
	"golang.org/x/sys/windows"
)

func (s *guestServer) executeNoTTY(ctx context.Context, stream pb.GuestService_ExecServer, start *pb.ExecStart) error {
	if len(start.Command) == 0 {
		return fmt.Errorf("empty command")
	}
	if start.Session != pb.ExecSession_EXEC_SESSION_DESKTOP {
		return s.executeSystemNoTTY(ctx, stream, start)
	}

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
	log.Printf("[guest-agent] desktop command finished with exit code: %d", exitCode)
	return stream.Send(&pb.ExecResponse{Response: &pb.ExecResponse_ExitCode{ExitCode: exitCode}})
}

func startDesktopProcess(
	ctx context.Context,
	command, env []string,
	cwd string,
	stdinRead, stdout, stderr *os.File,
	timeoutSeconds int32,
) (*os.Process, func(), error) {
	token, err := activeDesktopToken()
	if err != nil {
		return nil, nil, err
	}
	defer token.Close()

	commandLine, err := windows.UTF16PtrFromString(windows.ComposeCommandLine(command))
	if err != nil {
		return nil, nil, fmt.Errorf("encode desktop command: %w", err)
	}
	desktop, err := windows.UTF16PtrFromString(`winsta0\default`)
	if err != nil {
		return nil, nil, fmt.Errorf("encode desktop name: %w", err)
	}
	var currentDir *uint16
	if cwd != "" {
		currentDir, err = windows.UTF16PtrFromString(cwd)
		if err != nil {
			return nil, nil, fmt.Errorf("encode working directory: %w", err)
		}
	}
	envBlock, err := createWindowsEnvironmentBlock(env)
	if err != nil {
		return nil, nil, err
	}

	handles := []windows.Handle{
		windows.Handle(stdinRead.Fd()),
		windows.Handle(stdout.Fd()),
		windows.Handle(stderr.Fd()),
	}
	for _, handle := range handles {
		if err := windows.SetHandleInformation(handle, windows.HANDLE_FLAG_INHERIT, windows.HANDLE_FLAG_INHERIT); err != nil {
			return nil, nil, fmt.Errorf("make desktop command handle inheritable: %w", err)
		}
		defer windows.SetHandleInformation(handle, windows.HANDLE_FLAG_INHERIT, 0) //nolint:errcheck
	}

	attributes, err := windows.NewProcThreadAttributeList(1)
	if err != nil {
		return nil, nil, fmt.Errorf("create desktop command attribute list: %w", err)
	}
	defer attributes.Delete()
	if err := attributes.Update(
		windows.PROC_THREAD_ATTRIBUTE_HANDLE_LIST,
		unsafe.Pointer(&handles[0]),
		uintptr(len(handles))*unsafe.Sizeof(handles[0]),
	); err != nil {
		return nil, nil, fmt.Errorf("set desktop command handle list: %w", err)
	}

	startup := windows.StartupInfoEx{
		StartupInfo: windows.StartupInfo{
			Cb:        uint32(unsafe.Sizeof(windows.StartupInfoEx{})),
			Desktop:   desktop,
			Flags:     windows.STARTF_USESTDHANDLES,
			StdInput:  handles[0],
			StdOutput: handles[1],
			StdErr:    handles[2],
		},
		ProcThreadAttributeList: attributes.List(),
	}
	processInfo := windows.ProcessInformation{}
	flags := uint32(windows.CREATE_SUSPENDED | windows.CREATE_UNICODE_ENVIRONMENT | windows.CREATE_NO_WINDOW | windows.EXTENDED_STARTUPINFO_PRESENT)
	if err := windows.CreateProcessAsUser(
		token,
		nil,
		commandLine,
		nil,
		nil,
		true,
		flags,
		&envBlock[0],
		currentDir,
		&startup.StartupInfo,
		&processInfo,
	); err != nil {
		return nil, nil, fmt.Errorf("start desktop command: %w", err)
	}
	defer windows.CloseHandle(processInfo.Thread)
	defer windows.CloseHandle(processInfo.Process)

	process, err := os.FindProcess(int(processInfo.ProcessId))
	if err != nil {
		_ = windows.TerminateProcess(processInfo.Process, 1)
		return nil, nil, fmt.Errorf("open desktop command process: %w", err)
	}
	jobCleanup, err := attachProcessJob(ctx, process, time.Duration(timeoutSeconds)*time.Second)
	if err != nil {
		_ = windows.TerminateProcess(processInfo.Process, 1)
		_, _ = process.Wait()
		return nil, nil, fmt.Errorf("attach desktop process job: %w", err)
	}
	return process, jobCleanup, nil
}

func createWindowsEnvironmentBlock(env []string) ([]uint16, error) {
	seen := make(map[string]struct{}, len(env))
	deduplicated := make([]string, 0, len(env))
	for i := len(env) - 1; i >= 0; i-- {
		value := env[i]
		if strings.IndexByte(value, 0) >= 0 {
			return nil, fmt.Errorf("environment variable contains NUL")
		}
		separator := strings.IndexByte(value, '=')
		if separator == 0 {
			if next := strings.IndexByte(value[1:], '='); next >= 0 {
				separator = next + 1
			}
		}
		if separator < 0 {
			separator = len(value)
		}
		key := strings.ToLower(value[:separator])
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		deduplicated = append(deduplicated, value)
	}
	sort.Slice(deduplicated, func(i, j int) bool {
		return strings.ToLower(deduplicated[i]) < strings.ToLower(deduplicated[j])
	})
	return utf16.Encode([]rune(strings.Join(deduplicated, "\x00") + "\x00\x00")), nil
}
