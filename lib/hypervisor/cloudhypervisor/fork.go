package cloudhypervisor

import (
	"context"

	"github.com/kernel/hypeman/lib/forkvm"
	"github.com/kernel/hypeman/lib/hypervisor"
)

// PrepareFork prepares cloud-hypervisor fork state by rewriting snapshot config
// when a snapshot path is provided. For stopped forks (no snapshot), this is a no-op.
func (s *Starter) PrepareFork(ctx context.Context, req hypervisor.ForkPrepareRequest) error {
	_ = ctx
	if req.SnapshotConfigPath == "" {
		return nil
	}

	var netCfg *forkvm.SnapshotNetworkConfig
	if req.Network != nil {
		netCfg = &forkvm.SnapshotNetworkConfig{
			TAPDevice: req.Network.TAPDevice,
			IP:        req.Network.IP,
			MAC:       req.Network.MAC,
			Netmask:   req.Network.Netmask,
		}
	}

	return forkvm.RewriteSnapshotConfig(req.SnapshotConfigPath, forkvm.SnapshotRewriteOptions{
		SourceDataDir: req.SourceDataDir,
		TargetDataDir: req.TargetDataDir,
		VsockCID:      &req.VsockCID,
		VsockSocket:   req.VsockSocket,
		SerialLogPath: req.SerialLogPath,
		Network:       netCfg,
	})
}
