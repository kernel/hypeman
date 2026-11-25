package main

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// lockDataDir holds the directory lock until process exit. Background writers
// can outlive run(), so leave the raw descriptor open rather than deferring Close.
func lockDataDir(path string) error {
	if err := os.MkdirAll(path, 0755); err != nil {
		return err
	}
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	if err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		unix.Close(fd)
		if errors.Is(err, unix.EWOULDBLOCK) {
			return fmt.Errorf("another Hypeman process is using this directory: %w", err)
		}
		return err
	}
	return nil
}
