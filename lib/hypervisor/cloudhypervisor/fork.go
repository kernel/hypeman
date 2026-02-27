package cloudhypervisor

import (
	"context"

	"github.com/kernel/hypeman/lib/hypervisor"
)

// PrepareFork prepares cloud-hypervisor fork state by rewriting snapshot config
// when a snapshot path is provided. For stopped forks (no snapshot), this is a no-op.
func (s *Starter) PrepareFork(ctx context.Context, req hypervisor.ForkPrepareRequest) error {
	_ = ctx
	if req.SnapshotConfigPath == "" {
		return nil
	}

	return rewriteSnapshotConfigForFork(req.SnapshotConfigPath, req)
}
