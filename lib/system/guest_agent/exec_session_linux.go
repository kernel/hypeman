//go:build linux

package main

import (
	"fmt"
	"os/exec"

	pb "github.com/kernel/hypeman/lib/guest"
)

func defaultCommand() []string { return []string{"/bin/sh"} }

func configureExecCommand(_ *exec.Cmd, session pb.ExecSession) (func(), error) {
	switch session {
	case pb.ExecSession_EXEC_SESSION_SYSTEM:
		return func() {}, nil
	case pb.ExecSession_EXEC_SESSION_DESKTOP:
		return nil, fmt.Errorf("desktop execution sessions are only supported on Windows")
	default:
		return nil, fmt.Errorf("unknown execution session %d", session)
	}
}
