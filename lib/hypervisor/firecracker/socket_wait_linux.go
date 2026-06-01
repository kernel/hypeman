//go:build linux

package firecracker

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sys/unix"
)

func waitForSocket(path string, timeout time.Duration) error {
	if err := tryDialUnixSocket(path); err == nil {
		return nil
	}

	parent := filepath.Dir(path)
	fd, err := unix.InotifyInit1(unix.IN_CLOEXEC | unix.IN_NONBLOCK)
	if err != nil {
		return waitForSocketByPolling(path, timeout)
	}
	defer unix.Close(fd)

	wd, err := unix.InotifyAddWatch(fd, parent, unix.IN_CREATE|unix.IN_MOVED_TO|unix.IN_ATTRIB)
	if err != nil {
		return waitForSocketByPolling(path, timeout)
	}
	defer unix.InotifyRmWatch(fd, uint32(wd))

	deadline := time.Now().Add(timeout)
	buf := make([]byte, 4096)
	for {
		if err := tryDialUnixSocket(path); err == nil {
			return nil
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return fmt.Errorf("timeout waiting for socket")
		}

		pollTimeout := remaining
		if socketPathExists(path) {
			pollTimeout = minDuration(pollTimeout, socketReadyRetryEvery)
		}
		events := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
		n, err := unix.Poll(events, durationMillisCeil(pollTimeout))
		if err != nil {
			if err == unix.EINTR {
				continue
			}
			return waitForSocketByPolling(path, remaining)
		}
		if n > 0 {
			for {
				n, err := unix.Read(fd, buf)
				if err != nil {
					if err == unix.EAGAIN || err == unix.EWOULDBLOCK {
						break
					}
					return waitForSocketByPolling(path, time.Until(deadline))
				}
				if n == 0 {
					break
				}
			}
		}
	}
}

func socketPathExists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

func durationMillisCeil(d time.Duration) int {
	if d <= 0 {
		return 0
	}
	ms := d / time.Millisecond
	if d%time.Millisecond != 0 {
		ms++
	}
	if int64(ms) > int64(^uint(0)>>1) {
		return int(^uint(0) >> 1)
	}
	return int(ms)
}

func minDuration(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}
