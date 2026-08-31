//go:build !linux

package qemu

import (
	"errors"
	"os"
	"strconv"
	"syscall"
)

func swtpmProcessIdentity(pid int) (string, bool, error) {
	process, err := os.FindProcess(pid)
	if err != nil {
		return "", false, err
	}
	err = process.Signal(syscall.Signal(0))
	if err == nil || errors.Is(err, syscall.EPERM) {
		return strconv.Itoa(pid), true, nil
	}
	if errors.Is(err, os.ErrProcessDone) || errors.Is(err, syscall.ESRCH) {
		return "", false, nil
	}
	return "", false, err
}
