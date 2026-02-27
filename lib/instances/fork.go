package instances

import (
	"context"
	"errors"
	"fmt"
	"os"
	"regexp"
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
	stored := &meta.StoredMetadata

	switch source.State {
	case StateStopped, StateStandby:
		// allowed
	default:
		return nil, fmt.Errorf("%w: cannot fork from state %s (must be Stopped or Standby)", ErrInvalidState, source.State)
	}

	if stored.NetworkEnabled {
		exists, err := m.networkManager.NameExists(ctx, req.Name, id)
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
	forkMeta.VsockCID = generateVsockCID(forkID)
	forkMeta.VsockSocket = m.paths.InstanceVsockSocket(forkID)
	forkMeta.ExitCode = nil
	forkMeta.ExitMessage = ""

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
	} else {
		// Validate fork support for stopped-state forks as well.
		if err := starter.PrepareFork(ctx, hypervisor.ForkPrepareRequest{}); err != nil {
			if errors.Is(err, hypervisor.ErrNotSupported) {
				return nil, fmt.Errorf("%w: fork is not supported for hypervisor %s", ErrNotSupported, stored.HypervisorType)
			}
			return nil, fmt.Errorf("prepare fork state: %w", err)
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

func validateForkRequest(req ForkInstanceRequest) error {
	if req.Name == "" {
		return fmt.Errorf("name is required")
	}
	if len(req.Name) > 63 {
		return fmt.Errorf("name must be 63 characters or less")
	}
	namePattern := regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)
	if !namePattern.MatchString(req.Name) {
		return fmt.Errorf("name must contain only lowercase letters, digits, and dashes; cannot start or end with a dash")
	}
	return nil
}
