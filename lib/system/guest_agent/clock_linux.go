package main

import (
	"fmt"

	"golang.org/x/sys/unix"
)

func setRealtimeClock(unixNanos int64) error {
	ts := unix.NsecToTimespec(unixNanos)
	if err := unix.ClockSettime(unix.CLOCK_REALTIME, &ts); err != nil {
		return fmt.Errorf("set realtime clock: %w", err)
	}
	return nil
}
