//go:build darwin

package images

import (
	"syscall"
	"time"
)

func statAtime(stat *syscall.Stat_t) time.Time {
	return time.Unix(stat.Atimespec.Sec, stat.Atimespec.Nsec)
}
