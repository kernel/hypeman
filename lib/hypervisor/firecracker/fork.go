package firecracker

import (
	"context"
	"path/filepath"

	"github.com/kernel/hypeman/lib/hypervisor"
)

// PrepareFork updates Firecracker restore metadata for forked snapshots.
// Firecracker snapshot restore supports network overrides, but does not expose
// a public API for rewriting other snapshotted device paths.
// For standby/running forks, we persist source/target directory mapping so
// RestoreVM can temporarily alias source paths during snapshot load.
func (s *Starter) PrepareFork(ctx context.Context, req hypervisor.ForkPrepareRequest) (hypervisor.ForkPrepareResult, error) {
	_ = ctx
	if req.SnapshotConfigPath == "" {
		return hypervisor.ForkPrepareResult{}, nil
	}

	instanceDir := req.TargetDataDir
	if instanceDir == "" {
		// .../snapshots/snapshot-latest/config.json -> .../<instance-id>
		snapshotDir := filepath.Dir(req.SnapshotConfigPath)
		instanceDir = filepath.Dir(filepath.Dir(snapshotDir))
	}

	meta, err := loadRestoreMetadata(instanceDir)
	if err != nil {
		return hypervisor.ForkPrepareResult{}, err
	}

	changed := false
	if req.Network != nil && req.Network.TAPDevice != "" {
		if len(meta.NetworkOverrides) == 0 {
			meta.NetworkOverrides = []networkOverride{{
				IfaceID:     "eth0",
				HostDevName: req.Network.TAPDevice,
			}}
			changed = true
		} else {
			for i := range meta.NetworkOverrides {
				if meta.NetworkOverrides[i].HostDevName != req.Network.TAPDevice {
					meta.NetworkOverrides[i].HostDevName = req.Network.TAPDevice
					changed = true
				}
			}
		}
	}
	if req.SourceDataDir != "" && req.TargetDataDir != "" && req.SourceDataDir != req.TargetDataDir {
		if meta.SnapshotSourceDataDir != req.SourceDataDir {
			meta.SnapshotSourceDataDir = req.SourceDataDir
			changed = true
		}
	}

	if changed {
		if err := saveRestoreMetadataState(instanceDir, meta); err != nil {
			return hypervisor.ForkPrepareResult{}, err
		}
	}

	return hypervisor.ForkPrepareResult{}, nil
}
