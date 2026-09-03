//go:build linux

package images

import (
	"syscall"
	"time"
)

func statAtime(stat *syscall.Stat_t) time.Time {
	return time.Unix(stat.Atim.Sec, stat.Atim.Nsec)
}
