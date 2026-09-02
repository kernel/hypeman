package images

import (
	"errors"
	"fmt"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

func mknodWithRootlessFallback(path string, mode uint32, dev int) error {
	if err := unix.Mknod(path, mode, dev); err != nil {
		if !errors.Is(err, unix.EPERM) {
			return fmt.Errorf("mknod: %w", err)
		}
		if err := createRootlessDevicePlaceholder(path); err != nil {
			return err
		}
	}
	return nil
}

func createRootlessDevicePlaceholder(path string) error {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|syscall.O_NOFOLLOW, 0644)
	if err != nil {
		return fmt.Errorf("create rootless device placeholder: %w", err)
	}
	if err := file.Close(); err != nil {
		return err
	}
	return nil
}
