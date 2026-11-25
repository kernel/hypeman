package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

const dataDirLockName = ".hypeman.lock"

// lockDataDir holds the lock until process exit. Background writers can outlive
// run(), so leave the raw descriptor open on success. Never unlink the file:
// the next opener must lock the same inode.
func lockDataDir(path string) error {
	if err := os.MkdirAll(path, 0755); err != nil {
		return err
	}
	name := filepath.Join(path, dataDirLockName)
	fd, err := unix.Open(name, unix.O_CREAT|unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0600)
	if err != nil {
		return &os.PathError{Op: "open", Path: name, Err: err}
	}
	if err := lockPrivateFile(fd); err != nil {
		unix.Close(fd)
		return fmt.Errorf("%s: %w", name, err)
	}
	return nil
}

func lockPrivateFile(fd int) error {
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return err
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG || st.Mode&0077 != 0 || st.Uid != uint32(os.Geteuid()) {
		return errors.New("lock file must be a private regular file owned by the daemon user")
	}
	err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB)
	if errors.Is(err, unix.EWOULDBLOCK) {
		return fmt.Errorf("another Hypeman process is using this directory: %w", err)
	}
	return err
}
