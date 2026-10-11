//go:build darwin

package main

import (
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	"github.com/kernel/hypeman/lib/hypervisor/vz/shimconfig"
)

// Preserve the identity lock until subprocess exit, including while paused.
// Never unlink a lock file: another waiter may already hold its inode open.
func lockMacOSIdentity(c *shimconfig.ShimConfig) (*os.File, error) {
	if c.MacMachineIdentifierData == "" {
		return nil, nil
	}
	data, err := base64.StdEncoding.DecodeString(c.MacMachineIdentifierData)
	if err != nil {
		return nil, fmt.Errorf("decode Mac identity for admission: %w", err)
	}
	hash := sha256.Sum256(data)
	// Fixed directory, not TMPDIR: servers with different environments must contend on the same lock.
	path := filepath.Join("/tmp", fmt.Sprintf("hypeman-mac-identity-%d-%x.lock", os.Getuid(), hash))
	// The shared /tmp name is predictable, so refuse symlinks and foreign-owned files.
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	if st, ok := info.Sys().(*syscall.Stat_t); !ok || !info.Mode().IsRegular() || int(st.Uid) != os.Getuid() {
		f.Close()
		return nil, fmt.Errorf("Mac identity lock path is not an owned regular file")
	}
	if err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, fmt.Errorf("Mac machine identity already in use: %w", err)
	}
	return f, nil
}
