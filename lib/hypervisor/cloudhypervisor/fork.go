package cloudhypervisor

import (
	"context"

	"github.com/kernel/hypeman/lib/hypervisor"
)

// PrepareFork prepares cloud-hypervisor fork state by rewriting snapshot config
// when a snapshot path is provided. For stopped forks (no snapshot), this is a no-op.
func (s *Starter) PrepareFork(ctx context.Context, req hypervisor.ForkPrepareRequest) (hypervisor.ForkPrepareResult, error) {
	_ = ctx
	if req.SnapshotConfigPath == "" {
		return hypervisor.ForkPrepareResult{}, nil
	}

	if err := rewriteSnapshotConfigForFork(req.SnapshotConfigPath, req); err != nil {
		return hypervisor.ForkPrepareResult{}, err
	}
	return hypervisor.ForkPrepareResult{
		// CH snapshot restore keeps CID stable; only socket/path-level rewrites are applied.
		VsockCIDUpdated: false,
	}, nil
}
