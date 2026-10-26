//go:build darwin

package main

import (
	"errors"

	"golang.org/x/sys/unix"
)

// waitLeaderExit blocks until pid exits, without reaping it. kqueue reports NOTE_EXIT
// when the process exits, before it is reaped, so the pid stays reserved until the
// caller reaps it. A process that has already gone is reported as exited.
func waitLeaderExit(pid int) error {
	kq, err := unix.Kqueue()
	if err != nil {
		return err
	}
	defer unix.Close(kq)
	change := unix.Kevent_t{
		Ident:  uint64(pid),
		Filter: unix.EVFILT_PROC,
		Flags:  unix.EV_ADD | unix.EV_ONESHOT,
		Fflags: unix.NOTE_EXIT,
	}
	events := make([]unix.Kevent_t, 1)
	for {
		n, err := unix.Kevent(kq, []unix.Kevent_t{change}, events, nil)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			return err
		}
		if n > 0 && events[0].Flags&unix.EV_ERROR != 0 {
			if errno := unix.Errno(events[0].Data); errno != unix.ESRCH {
				return errno
			}
		}
		return nil
	}
}
