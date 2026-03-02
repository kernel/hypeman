package instances

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"hash/crc32"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/kernel/hypeman/lib/forkvm"
	"github.com/kernel/hypeman/lib/guest"
	"github.com/kernel/hypeman/lib/hypervisor"
	"github.com/kernel/hypeman/lib/logger"
	"github.com/kernel/hypeman/lib/network"
	"github.com/nrednav/cuid2"
	"gvisor.dev/gvisor/pkg/cleanup"
)

// forkInstance creates a new instance by cloning a stopped or standby source
// instance. It returns the newly created fork and the requested final target
// state; callers apply remaining target state transitions outside the source lock.
func (m *manager) forkInstance(ctx context.Context, id string, req ForkInstanceRequest) (*Instance, State, error) {
	log := logger.FromContext(ctx)
	log.InfoContext(ctx, "forking instance", "source_instance_id", id, "fork_name", req.Name)

	if err := validateForkRequest(req); err != nil {
		return nil, "", err
	}

	meta, err := m.loadMetadata(id)
	if err != nil {
		return nil, "", err
	}
	source := m.toInstance(ctx, meta)
	targetState, err := resolveForkTargetState(req.TargetState, source.State)
	if err != nil {
		return nil, "", err
	}

	switch source.State {
	case StateRunning:
		if !req.FromRunning {
			return nil, "", fmt.Errorf("%w: cannot fork from state %s (set from_running=true to allow standby+restore flow)", ErrInvalidState, source.State)
		}

		if err := m.validateForkSupport(ctx, source.HypervisorType); err != nil {
			return nil, "", err
		}
		if err := ensureGuestAgentReadyForRunningFork(ctx, &source.StoredMetadata); err != nil {
			return nil, "", err
		}

		log.InfoContext(ctx, "fork from running requested; transitioning source to standby",
			"source_instance_id", id, "hypervisor", source.HypervisorType)
		if _, err := m.standbyInstance(ctx, id); err != nil {
			return nil, "", fmt.Errorf("standby source instance: %w", err)
		}

		forked, forkErr := m.forkInstanceFromStoppedOrStandby(ctx, id, req, true)
		if forkErr == nil {
			if err := m.rotateSourceVsockForRestore(ctx, id, forked.Id); err != nil {
				forkErr = fmt.Errorf("prepare source snapshot for restore: %w", err)
				if cleanupErr := m.cleanupForkInstanceOnError(ctx, forked.Id); cleanupErr != nil {
					forkErr = fmt.Errorf("%v; additionally failed to cleanup forked instance %s: %v", forkErr, forked.Id, cleanupErr)
				}
			}
		}

		// For Firecracker running-source forks, restoring the fork may temporarily alias
		// the source data directory. Restore the fork while source remains standby and
		// under lock, then restore the source.
		if forkErr == nil && targetState == StateRunning {
			restoredFork, err := m.applyForkTargetState(ctx, forked.Id, StateRunning)
			if err != nil {
				forkErr = fmt.Errorf("restore forked instance before source restore: %w", err)
				if cleanupErr := m.cleanupForkInstanceOnError(ctx, forked.Id); cleanupErr != nil {
					forkErr = fmt.Errorf("%v; additionally failed to cleanup forked instance %s: %v", forkErr, forked.Id, cleanupErr)
				}
			} else {
				forked = restoredFork
			}
		}

		log.InfoContext(ctx, "restoring source instance after running fork", "source_instance_id", id)
		_, restoreErr := m.restoreInstance(ctx, id)

		if restoreErr != nil {
			if forkErr != nil {
				return nil, "", fmt.Errorf("fork failed: %v; additionally failed to restore source instance: %w", forkErr, restoreErr)
			}
			return nil, "", fmt.Errorf("restore source instance after fork: %w", restoreErr)
		}
		if forkErr != nil {
			return nil, "", forkErr
		}
		return forked, targetState, nil
	case StateStopped, StateStandby:
		forked, err := m.forkInstanceFromStoppedOrStandby(ctx, id, req, false)
		if err != nil {
			return nil, "", err
		}
		return forked, targetState, nil
	default:
		return nil, "", fmt.Errorf("%w: cannot fork from state %s (must be Stopped or Standby, or Running with from_running=true)", ErrInvalidState, source.State)
	}
}

func ensureGuestAgentReadyForRunningFork(ctx context.Context, source *StoredMetadata) error {
	if source == nil || !source.NetworkEnabled || source.SkipGuestAgent {
		return nil
	}

	dialer, err := hypervisor.NewVsockDialer(source.HypervisorType, source.VsockSocket, source.VsockCID)
	if err != nil {
		return fmt.Errorf("create vsock dialer for running fork readiness check: %w", err)
	}

	var stdout, stderr bytes.Buffer
	exit, err := guest.ExecIntoInstance(ctx, dialer, guest.ExecOptions{
		Command:      []string{"true"},
		Stdout:       &stdout,
		Stderr:       &stderr,
		WaitForAgent: 120 * time.Second,
	})
	if err != nil {
		return fmt.Errorf("wait for guest agent readiness before running fork: %w", err)
	}
	if exit.Code != 0 {
		return fmt.Errorf(
			"guest agent readiness probe failed before running fork (exit=%d, stdout=%q, stderr=%q)",
			exit.Code, strings.TrimSpace(stdout.String()), strings.TrimSpace(stderr.String()),
		)
	}
	return nil
}

func (m *manager) rotateSourceVsockForRestore(ctx context.Context, sourceID, forkID string) error {
	meta, err := m.loadMetadata(sourceID)
	if err != nil {
		return fmt.Errorf("reload source metadata: %w", err)
	}
	stored := &meta.StoredMetadata

	newCID := generateForkSourceVsockCID(sourceID, forkID, stored.VsockCID)
	if newCID == stored.VsockCID {
		return nil
	}

	starter, err := m.getVMStarter(stored.HypervisorType)
	if err != nil {
		return fmt.Errorf("get vm starter: %w", err)
	}

	prepareResult, err := starter.PrepareFork(ctx, hypervisor.ForkPrepareRequest{
		SnapshotConfigPath: m.paths.InstanceSnapshotConfig(sourceID),
		VsockCID:           newCID,
		VsockSocket:        stored.VsockSocket,
	})
	if err != nil {
		return fmt.Errorf("rewrite source snapshot vsock state: %w", err)
	}

	if prepareResult.VsockCIDUpdated {
		stored.VsockCID = newCID
		if err := m.saveMetadata(meta); err != nil {
			return fmt.Errorf("save source metadata: %w", err)
		}
	}
	return nil
}

func generateForkSourceVsockCID(sourceID, forkID string, current int64) int64 {
	const cidRange = int64(4294967292)
	seed := crc32.ChecksumIEEE([]byte(sourceID + ":" + forkID))
	cid := (int64(seed) % cidRange) + 3
	if cid == current {
		cid = ((cid - 3 + 1) % cidRange) + 3
	}
	return cid
}

func (m *manager) forkInstanceFromStoppedOrStandby(ctx context.Context, id string, req ForkInstanceRequest, supportValidated bool) (*Instance, error) {
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

	if !supportValidated {
		if err := m.validateForkSupport(ctx, stored.HypervisorType); err != nil {
			return nil, err
		}
	}
	if err := validateForkVolumeSafety(stored.Volumes); err != nil {
		return nil, err
	}

	existsByMetadata, err := m.instanceNameExists(req.Name)
	if err != nil {
		return nil, fmt.Errorf("check instance name availability: %w", err)
	}
	if existsByMetadata {
		return nil, fmt.Errorf("%w: instance name '%s' already exists", ErrAlreadyExists, req.Name)
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
	forkMeta := cloneStoredMetadataForFork(meta.StoredMetadata)
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

	// Keep the original CID for snapshot-based forks.
	// Rewriting CID in restored memory snapshots is not reliable across
	// hypervisors.
	if source.State == StateStandby {
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
		if _, err := starter.PrepareFork(ctx, hypervisor.ForkPrepareRequest{
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
	if _, err := starter.PrepareFork(ctx, hypervisor.ForkPrepareRequest{}); err != nil {
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
	if req.TargetState != "" && req.TargetState != StateStopped && req.TargetState != StateStandby && req.TargetState != StateRunning {
		return fmt.Errorf("%w: invalid fork target state %q (must be one of %s, %s, %s)", ErrInvalidRequest, req.TargetState, StateStopped, StateStandby, StateRunning)
	}
	return nil
}

func validateForkVolumeSafety(volumes []VolumeAttachment) error {
	for _, vol := range volumes {
		if !vol.Readonly {
			return fmt.Errorf("%w: cannot fork instance with writable volume %q mounted at %q; use readonly+overlay for safe concurrent forks", ErrNotSupported, vol.VolumeID, vol.MountPath)
		}
	}
	return nil
}

func (m *manager) instanceNameExists(name string) (bool, error) {
	metaFiles, err := m.listMetadataFiles()
	if err != nil {
		return false, err
	}

	for _, metaFile := range metaFiles {
		id := filepath.Base(filepath.Dir(metaFile))
		meta, err := m.loadMetadata(id)
		if err != nil {
			continue
		}
		if meta.Name == name {
			return true, nil
		}
	}
	return false, nil
}

func resolveForkTargetState(requested State, sourceState State) (State, error) {
	if requested == "" {
		switch sourceState {
		case StateRunning, StateStandby, StateStopped:
			return sourceState, nil
		default:
			return "", fmt.Errorf("%w: cannot derive fork target state from source state %s", ErrInvalidState, sourceState)
		}
	}
	return requested, nil
}

func (m *manager) applyForkTargetState(ctx context.Context, forkID string, target State) (*Instance, error) {
	lock := m.getInstanceLock(forkID)
	lock.Lock()
	defer lock.Unlock()

	current, err := m.getInstance(ctx, forkID)
	if err != nil {
		return nil, err
	}
	if current.State == target {
		return current, nil
	}

	switch current.State {
	case StateStopped:
		switch target {
		case StateRunning:
			return m.startInstance(ctx, forkID, StartInstanceRequest{})
		case StateStandby:
			if _, err := m.startInstance(ctx, forkID, StartInstanceRequest{}); err != nil {
				return nil, fmt.Errorf("start forked instance for standby transition: %w", err)
			}
			return m.standbyInstance(ctx, forkID)
		}
	case StateStandby:
		switch target {
		case StateRunning:
			return m.restoreInstance(ctx, forkID)
		case StateStopped:
			if err := os.RemoveAll(m.paths.InstanceSnapshotLatest(forkID)); err != nil {
				return nil, fmt.Errorf("remove fork snapshot: %w", err)
			}
			return m.getInstance(ctx, forkID)
		}
	case StateRunning:
		switch target {
		case StateStandby:
			return m.standbyInstance(ctx, forkID)
		case StateStopped:
			return m.stopInstance(ctx, forkID)
		}
	}

	return nil, fmt.Errorf("%w: cannot transition forked instance from %s to %s", ErrInvalidState, current.State, target)
}

func (m *manager) cleanupForkInstanceOnError(ctx context.Context, forkID string) error {
	lock := m.getInstanceLock(forkID)
	lock.Lock()
	defer lock.Unlock()

	err := m.deleteInstance(ctx, forkID)
	if err == nil || errors.Is(err, ErrNotFound) {
		m.instanceLocks.Delete(forkID)
		return nil
	}
	return err
}

func cloneStoredMetadataForFork(src StoredMetadata) StoredMetadata {
	dst := src

	if src.Env != nil {
		dst.Env = make(map[string]string, len(src.Env))
		for k, v := range src.Env {
			dst.Env[k] = v
		}
	}
	if src.Metadata != nil {
		dst.Metadata = make(map[string]string, len(src.Metadata))
		for k, v := range src.Metadata {
			dst.Metadata[k] = v
		}
	}
	if src.Volumes != nil {
		dst.Volumes = append([]VolumeAttachment(nil), src.Volumes...)
	}
	if src.Devices != nil {
		dst.Devices = append([]string(nil), src.Devices...)
	}
	if src.Entrypoint != nil {
		dst.Entrypoint = append([]string(nil), src.Entrypoint...)
	}
	if src.Cmd != nil {
		dst.Cmd = append([]string(nil), src.Cmd...)
	}
	if src.HypervisorPID != nil {
		pid := *src.HypervisorPID
		dst.HypervisorPID = &pid
	}
	if src.StartedAt != nil {
		startedAt := *src.StartedAt
		dst.StartedAt = &startedAt
	}
	if src.StoppedAt != nil {
		stoppedAt := *src.StoppedAt
		dst.StoppedAt = &stoppedAt
	}
	if src.ExitCode != nil {
		exitCode := *src.ExitCode
		dst.ExitCode = &exitCode
	}

	return dst
}
