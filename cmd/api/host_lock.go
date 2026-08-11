package main

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"syscall"
)

const hostLockFilename = ".hypeman.lock"

func acquireHostLock(dataDir string) (*os.File, error) {
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return nil, fmt.Errorf("create data directory: %w", err)
	}

	path := filepath.Join(dataDir, hostLockFilename)
	lock, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open host lock: %w", err)
	}

	slog.Info("waiting for host ownership", "path", path)
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		_ = lock.Close()
		return nil, fmt.Errorf("acquire host lock: %w", err)
	}
	slog.Info("acquired host ownership", "path", path)
	return lock, nil
}

func releaseHostLock(lock *os.File) error {
	return errors.Join(
		syscall.Flock(int(lock.Fd()), syscall.LOCK_UN),
		lock.Close(),
	)
}
