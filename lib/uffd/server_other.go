//go:build !linux

package uffd

import "context"

// startListener returns ErrUnsupported on non-Linux platforms.
// userfaultfd is a Linux-only kernel feature; callers should fall back
// to letting firecracker mmap the mem-file privately.
func (s *Server) startListener(ctx context.Context, forkID string, socketPath string) (func() error, error) {
	_ = ctx
	_ = forkID
	_ = socketPath
	return nil, ErrUnsupported
}
