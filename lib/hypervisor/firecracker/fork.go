package firecracker

import (
	"context"

	"github.com/kernel/hypeman/lib/hypervisor"
)

// PrepareFork reports fork capability for Firecracker.
//
// Firecracker supports forking from Stopped sources (no snapshot rewrite).
// Snapshot-based fork rewrites (Standby/Running fork flows) are currently not
// supported by the Firecracker restore API.
func (s *Starter) PrepareFork(ctx context.Context, req hypervisor.ForkPrepareRequest) (hypervisor.ForkPrepareResult, error) {
	_ = ctx
	if req.SnapshotConfigPath != "" {
		return hypervisor.ForkPrepareResult{}, hypervisor.ErrNotSupported
	}
	return hypervisor.ForkPrepareResult{}, nil
}
