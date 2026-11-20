package main

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

// Instance metadata locks are process-local. Hold the directory inode locked
// until shutdown, before any startup scan can read and write instance records.
func lockDataDir(path string) (*os.File, error) {
	if err := os.MkdirAll(path, 0755); err != nil {
		return nil, err
	}
	dir, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(dir.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		dir.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, fmt.Errorf("another Hypeman process is using this directory: %w", err)
		}
		return nil, err
	}
	return dir, nil
}
