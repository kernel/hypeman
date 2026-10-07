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
	path := filepath.Join(os.TempDir(), fmt.Sprintf("hypeman-mac-identity-%d-%x.lock", os.Getuid(), hash))
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, fmt.Errorf("Mac machine identity already in use: %w", err)
	}
	return f, nil
}
