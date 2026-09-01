//go:build !linux

package qemu

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"syscall"

	"github.com/kernel/hypeman/lib/hypervisor"
)

func discoverSWTPMProcess(config *hypervisor.TPMConfig) (swtpmProcessRecord, bool, error) {
	if _, err := os.Lstat(config.SocketPath); err == nil {
		return swtpmProcessRecord{}, false, fmt.Errorf("cannot verify owner of existing socket %s on this platform", config.SocketPath)
	} else if !os.IsNotExist(err) {
		return swtpmProcessRecord{}, false, err
	}
	return swtpmProcessRecord{}, false, nil
}

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
