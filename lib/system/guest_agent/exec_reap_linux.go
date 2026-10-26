//go:build linux

package main

import "golang.org/x/sys/unix"

// waitLeaderExit blocks until pid exits, without reaping it. The pid stays reserved
// until the caller reaps it, so a process group keyed by that pid stays addressable.
func waitLeaderExit(pid int) error {
	for {
		var info unix.Siginfo
		err := unix.Waitid(unix.P_PID, pid, &info, unix.WEXITED|unix.WNOWAIT, nil)
		if err != unix.EINTR {
			return err
		}
	}
}
