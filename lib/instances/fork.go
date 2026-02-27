package instances

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/kernel/hypeman/lib/forkvm"
	"github.com/kernel/hypeman/lib/hypervisor"
	"github.com/kernel/hypeman/lib/logger"
	"github.com/kernel/hypeman/lib/network"
	"github.com/nrednav/cuid2"
	"gvisor.dev/gvisor/pkg/cleanup"
)

// forkInstance creates a new instance by cloning a stopped or standby source instance.
func (m *manager) forkInstance(ctx context.Context, id string, req ForkInstanceRequest) (*Instance, error) {
	log := logger.FromContext(ctx)
	log.InfoContext(ctx, "forking instance", "source_instance_id", id, "fork_name", req.Name)

	if err := validateForkRequest(req); err != nil {
		return nil, err
	}

	meta, err := m.loadMetadata(id)
	if err != nil {
		return nil, err
	}
	source := m.toInstance(ctx, meta)

	switch source.State {
	case StateRunning:
		if !req.FromRunning {
			return nil, fmt.Errorf("%w: cannot fork from state %s (set from_running=true to allow standby+restore flow)", ErrInvalidState, source.State)
		}

		if err := m.validateForkSupport(ctx, source.HypervisorType); err != nil {
			return nil, err
		}

		log.InfoContext(ctx, "fork from running requested; transitioning source to standby",
			"source_instance_id", id, "hypervisor", source.HypervisorType)
		if _, err := m.standbyInstance(ctx, id); err != nil {
			return nil, fmt.Errorf("standby source instance: %w", err)
		}

		forked, forkErr := m.forkInstanceFromStoppedOrStandby(ctx, id, req)
		log.InfoContext(ctx, "restoring source instance after running fork", "source_instance_id", id)
		_, restoreErr := m.restoreInstance(ctx, id)

		if restoreErr != nil {
			if forkErr != nil {
				return nil, fmt.Errorf("fork failed: %v; additionally failed to restore source instance: %w", forkErr, restoreErr)
			}
			return nil, fmt.Errorf("restore source instance after fork: %w", restoreErr)
		}
		if forkErr != nil {
			return nil, forkErr
		}
		return forked, nil
	case StateStopped, StateStandby:
		return m.forkInstanceFromStoppedOrStandby(ctx, id, req)
	default:
		return nil, fmt.Errorf("%w: cannot fork from state %s (must be Stopped or Standby, or Running with from_running=true)", ErrInvalidState, source.State)
	}
}

func (m *manager) forkInstanceFromStoppedOrStandby(ctx context.Context, id string, req ForkInstanceRequest) (*Instance, error) {
	log := logger.FromContext(ctx)

	meta, err := m.loadMetadata(id)
	if err != nil {
		return nil, err
	}

	source := m.toInstance(ctx, meta)
	stored := &meta.StoredMetadata

	switch source.State {
	case StateStopped, StateStandby:
		// allowed
	default:
		return nil, fmt.Errorf("%w: cannot fork from state %s (must be Stopped or Standby)", ErrInvalidState, source.State)
	}

	if err := m.validateForkSupport(ctx, stored.HypervisorType); err != nil {
		return nil, err
	}

	if stored.NetworkEnabled {
		exists, err := m.networkManager.NameExists(ctx, req.Name, "")
		if err != nil {
			return nil, fmt.Errorf("check instance name availability: %w", err)
		}
		if exists {
			return nil, fmt.Errorf("%w: instance name '%s' already exists in network", ErrAlreadyExists, req.Name)
		}
	}

	forkID := cuid2.Generate()
	if _, err := m.loadMetadata(forkID); err == nil {
		return nil, fmt.Errorf("%w: generated fork id already exists", ErrAlreadyExists)
	}

	srcDir := m.paths.InstanceDir(id)
	dstDir := m.paths.InstanceDir(forkID)

	cu := cleanup.Make(func() {
		_ = os.RemoveAll(dstDir)
	})
	defer cu.Clean()

	if err := forkvm.CopyGuestDirectory(srcDir, dstDir); err != nil {
		return nil, fmt.Errorf("clone guest directory: %w", err)
	}

	starter, err := m.getVMStarter(stored.HypervisorType)
	if err != nil {
		return nil, fmt.Errorf("get vm starter: %w", err)
	}

	now := time.Now()
	forkMeta := meta.StoredMetadata
	forkMeta.Id = forkID
	forkMeta.Name = req.Name
	forkMeta.CreatedAt = now
	forkMeta.StartedAt = nil
	forkMeta.StoppedAt = nil
	forkMeta.HypervisorPID = nil
	forkMeta.SocketPath = m.paths.InstanceSocket(forkID, starter.SocketName())
	forkMeta.DataDir = dstDir
	forkMeta.VsockSocket = m.paths.InstanceVsockSocket(forkID)
	forkMeta.ExitCode = nil
	forkMeta.ExitMessage = ""

	if source.State == StateStandby {
		// Keep the original CID for snapshot-based forks.
		// Rewriting CID in restored memory snapshots is not reliable for CH.
		forkMeta.VsockCID = stored.VsockCID
	} else {
		forkMeta.VsockCID = generateVsockCID(forkID)
	}

	if forkMeta.NetworkEnabled {
		// Clear inherited network identity. For stopped instances this is regenerated on start,
		// and for standby instances restore allocates if identity is empty.
		forkMeta.IP = ""
		forkMeta.MAC = ""
	}

	if source.State == StateStandby {
		snapshotConfigPath := m.paths.InstanceSnapshotConfig(forkID)
		netCfg := (*hypervisor.ForkNetworkConfig)(nil)
		if forkMeta.NetworkEnabled {
			netCfg = &hypervisor.ForkNetworkConfig{TAPDevice: network.GenerateTAPName(forkID)}
		}
		if err := starter.PrepareFork(ctx, hypervisor.ForkPrepareRequest{
			SnapshotConfigPath: snapshotConfigPath,
			SourceDataDir:      stored.DataDir,
			TargetDataDir:      forkMeta.DataDir,
			VsockCID:           forkMeta.VsockCID,
			VsockSocket:        forkMeta.VsockSocket,
			SerialLogPath:      m.paths.InstanceAppLog(forkID),
			Network:            netCfg,
		}); err != nil {
			if errors.Is(err, hypervisor.ErrNotSupported) {
				return nil, fmt.Errorf("%w: fork is not supported for hypervisor %s", ErrNotSupported, stored.HypervisorType)
			}
			return nil, fmt.Errorf("prepare fork snapshot state: %w", err)
		}
	}

	newMeta := &metadata{StoredMetadata: forkMeta}
	if err := m.saveMetadata(newMeta); err != nil {
		return nil, fmt.Errorf("save fork metadata: %w", err)
	}

	cu.Release()
	forked := m.toInstance(ctx, newMeta)
	log.InfoContext(ctx, "instance forked successfully",
		"source_instance_id", id,
		"fork_instance_id", forked.Id,
		"fork_name", forked.Name,
		"state", forked.State)
	return &forked, nil
}

func (m *manager) validateForkSupport(ctx context.Context, hvType hypervisor.Type) error {
	starter, err := m.getVMStarter(hvType)
	if err != nil {
		return fmt.Errorf("get vm starter: %w", err)
	}
	if err := starter.PrepareFork(ctx, hypervisor.ForkPrepareRequest{}); err != nil {
		if errors.Is(err, hypervisor.ErrNotSupported) {
			return fmt.Errorf("%w: fork is not supported for hypervisor %s", ErrNotSupported, hvType)
		}
		return fmt.Errorf("prepare fork state: %w", err)
	}
	return nil
}

func validateForkRequest(req ForkInstanceRequest) error {
	if err := validateInstanceName(req.Name); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidRequest, err)
	}
	return nil
}
