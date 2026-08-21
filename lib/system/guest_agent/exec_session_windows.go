//go:build windows

package main

import (
	"context"
	"fmt"
	"os/exec"
	"syscall"
	"time"
	"unsafe"

	pb "github.com/kernel/hypeman/lib/guest"
	"golang.org/x/sys/windows"
)

func defaultCommand() []string { return []string{"cmd.exe"} }

func execContext(streamCtx context.Context) context.Context { return streamCtx }

func execCommand(_ context.Context, name string, args ...string) *exec.Cmd {
	return exec.Command(name, args...)
}

func activeDesktopToken() (windows.Token, error) {
	var sessions *windows.WTS_SESSION_INFO
	var count uint32
	if err := windows.WTSEnumerateSessions(0, 0, 1, &sessions, &count); err != nil {
		return 0, fmt.Errorf("enumerate desktop sessions: %w", err)
	}
	if sessions != nil {
		defer windows.WTSFreeMemory(uintptr(unsafe.Pointer(sessions)))
	}

	for _, session := range unsafe.Slice(sessions, count) {
		if session.State != windows.WTSActive {
			continue
		}
		var token windows.Token
		if err := windows.WTSQueryUserToken(session.SessionID, &token); err == nil {
			return token, nil
		}
	}
	return 0, fmt.Errorf("no active desktop user session")
}

func executionToken(session pb.ExecSession) (syscall.Token, func(), error) {
	switch session {
	case pb.ExecSession_EXEC_SESSION_SYSTEM:
		return 0, func() {}, nil
	case pb.ExecSession_EXEC_SESSION_DESKTOP:
	default:
		return 0, nil, fmt.Errorf("unknown execution session %d", session)
	}
	token, err := activeDesktopToken()
	if err != nil {
		return 0, nil, err
	}
	return syscall.Token(token), func() { _ = token.Close() }, nil
}

func configureExecCommand(cmd *exec.Cmd, session pb.ExecSession) (func(), error) {
	token, cleanup, err := executionToken(session)
	if err != nil {
		return nil, err
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Token:         token,
		CreationFlags: windows.CREATE_SUSPENDED,
	}
	cmd.WaitDelay = 2 * time.Second
	return cleanup, nil
}
