package instances

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"hash/crc32"
	"os"
	"strings"
	"time"

	"github.com/kernel/hypeman/lib/guest"
	"github.com/kernel/hypeman/lib/hypervisor"
	"github.com/kernel/hypeman/lib/instances/phasetracking"
	"github.com/kernel/hypeman/lib/logger"
	"github.com/kernel/hypeman/lib/network"
	restartpolicy "github.com/kernel/hypeman/lib/restart-policy"
	"github.com/nrednav/cuid2"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"gvisor.dev/gvisor/pkg/cleanup"
)

// forkInstance creates a new instance by cloning a stopped or standby source
// instance. It returns the newly created fork and the requested final target
// state; callers apply remaining target state transitions outside the source lock.
func (m *manager) forkInstance(ctx context.Context, id string, req ForkInstanceRequest) (_ *Instance, _ State, _ bool, retErr error) {
	log := logger.FromContext(ctx)
	log.InfoContext(ctx, "forking instance", "source_instance_id", id, "fork_name", req.Name)
	ctx, span := m.startLifecycleSpan(ctx, "instances.fork",
		attribute.String("instance_id", id),
		attribute.String("operation", "fork"),
	)
	defer func() { finishInstancesSpan(span, retErr) }()

	if err := validateForkRequest(req); err != nil {
		return nil, "", false, err
	}

	meta, err := m.loadMetadata(id)
	if err != nil {
		return nil, "", false, err
	}
	source := m.toInstance(ctx, meta)
	targetState, err := resolveForkTargetState(req.TargetState, source.State)
	if err != nil {
		return nil, "", false, err
	}
	if isWindowsPlatform(source.Platform) && source.State == StateRunning && targetState != StateStopped {
		return nil, "", false, fmt.Errorf("%w: Windows forks from a running source require target_state=%s", ErrNotSupported, StateStopped)
	}

	switch source.State {
	case StateRunning:
		if !req.FromRunning {
			return nil, "", false, fmt.Errorf("%w: cannot fork from state %s (set from_running=true to allow standby+restore flow)", ErrInvalidState, source.State)
		}

		if err := m.validateForkSupport(ctx, source.HypervisorType); err != nil {
			return nil, "", false, err
		}
		if err := ensureGuestAgentReadyForRunningFork(ctx, &source.StoredMetadata); err != nil {
			return nil, "", false, err
		}

		log.InfoContext(ctx, "fork from running requested; transitioning source to standby",
			"source_instance_id", id, "hypervisor", source.HypervisorType)
		if _, err := m.standbyInstance(ctx, id, StandbyInstanceRequest{}, true); err != nil {
			return nil, "", false, fmt.Errorf("standby source instance: %w", err)
		}

		forked, _, forkErr := m.forkInstanceFromStoppedOrStandby(ctx, id, req, true)
		if forkErr == nil && targetState != StateStopped {
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
		restoredSource, restoreErr := m.restoreInstance(ctx, id)

		if restoreErr != nil {
			if forkErr != nil {
				return nil, "", false, fmt.Errorf("fork failed: %v; additionally failed to restore source instance: %w", forkErr, restoreErr)
			}
			return nil, "", false, fmt.Errorf("restore source instance after fork: %w", restoreErr)
		}
		if restoredSource != nil && !restoredSource.NetworkEnabled && (restoredSource.State == StateRunning || restoredSource.State == StateInitializing) {
			if err := ensureGuestAgentReadyForForkPhase(ctx, &restoredSource.StoredMetadata, "after restoring running fork source"); err != nil {
				if forkErr != nil {
					return nil, "", false, fmt.Errorf("fork failed: %v; additionally restored source guest agent was not ready: %w", forkErr, err)
				}
				return nil, "", false, fmt.Errorf("wait for restored source guest agent readiness: %w", err)
			}
		}
		if forkErr != nil {
			return nil, "", false, forkErr
		}
		return forked, targetState, false, nil
	case StateStopped, StateStandby:
		forked, restoreNeedsSourceLock, err := m.forkInstanceFromStoppedOrStandby(ctx, id, req, false)
		if err != nil {
			return nil, "", false, err
		}
		return forked, targetState, restoreNeedsSourceLock, nil
	default:
		return nil, "", false, fmt.Errorf("%w: cannot fork from state %s (must be Stopped or Standby, or Running with from_running=true)", ErrInvalidState, source.State)
	}
}

func ensureGuestAgentReadyForRunningFork(ctx context.Context, source *StoredMetadata) error {
	return ensureGuestAgentReadyForForkPhase(ctx, source, "before running fork")
}

func ensureGuestAgentReadyForForkPhase(ctx context.Context, inst *StoredMetadata, phase string) error {
	if inst == nil || inst.SkipGuestAgent {
		return nil
	}

	dialer, err := hypervisor.NewVsockDialer(inst.HypervisorType, inst.VsockSocket, inst.VsockCID)
	if err != nil {
		return fmt.Errorf("create vsock dialer for %s readiness check: %w", phase, err)
	}

	command := []string{"true"}
	if isWindowsPlatform(inst.Platform) {
		command = []string{"cmd.exe", "/d", "/c", "exit", "0"}
	}
	var stdout, stderr bytes.Buffer
	exit, err := guest.ExecIntoInstance(ctx, dialer, guest.ExecOptions{
		Command:      command,
		Stdout:       &stdout,
		Stderr:       &stderr,
		WaitForAgent: 120 * time.Second,
	})
	if err != nil {
		return fmt.Errorf("wait for guest agent readiness %s: %w", phase, err)
	}
	if exit.Code != 0 {
		return fmt.Errorf(
			"guest agent readiness probe failed %s (exit=%d, stdout=%q, stderr=%q)",
			phase, exit.Code, strings.TrimSpace(stdout.String()), strings.TrimSpace(stderr.String()),
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

func (m *manager) forkInstanceFromStoppedOrStandby(ctx context.Context, id string, req ForkInstanceRequest, supportValidated bool) (*Instance, bool, error) {
	log := logger.FromContext(ctx)

	meta, err := m.loadMetadata(id)
	if err != nil {
		return nil, false, err
	}

	source := m.toInstance(ctx, meta)
	stored := &meta.StoredMetadata
	if err := validateWindowsForkPolicy(stored); err != nil {
		return nil, false, err
	}

	switch source.State {
	case StateStopped, StateStandby:
		// allowed
	default:
		return nil, false, fmt.Errorf("%w: cannot fork from state %s (must be Stopped or Standby)", ErrInvalidState, source.State)
	}

	if !supportValidated {
		if err := m.validateForkSupport(ctx, stored.HypervisorType); err != nil {
			return nil, false, err
		}
	}
	targetState, err := resolveForkTargetState(req.TargetState, source.State)
	if err != nil {
		return nil, false, err
	}
	if err := validateForkVolumeSafety(stored.Volumes); err != nil {
		return nil, false, err
	}

	existsByMetadata, err := m.instanceNameExists(ctx, req.Name, "fork_precheck")
	if err != nil {
		return nil, false, fmt.Errorf("check instance name availability: %w", err)
	}
	if existsByMetadata {
		return nil, false, fmt.Errorf("%w: instance name '%s' already exists", ErrAlreadyExists, req.Name)
	}
	if stored.NetworkEnabled {
		exists, err := m.networkManager.NameExists(ctx, req.Name, "")
		if err != nil {
			return nil, false, fmt.Errorf("check instance name availability: %w", err)
		}
		if exists {
			return nil, false, fmt.Errorf("%w: instance name '%s' already exists in network", ErrAlreadyExists, req.Name)
		}
	}

	forkID := cuid2.Generate()
	if _, err := m.loadMetadata(forkID); err == nil {
		return nil, false, fmt.Errorf("%w: generated fork id already exists", ErrAlreadyExists)
	}

	srcDir := m.paths.InstanceDir(id)
	dstDir := m.paths.InstanceDir(forkID)
	shareMemFile := stored.HypervisorType == hypervisor.TypeFirecracker && source.State == StateStandby

	cu := cleanup.Make(func() {
		_ = os.RemoveAll(dstDir)
	})
	defer cu.Clean()

	if err := m.copyForkSourceGuestDirectory(ctx, source.State, id, stored, srcDir, dstDir, shareMemFile); err != nil {
		return nil, false, err
	}

	starter, err := m.getVMStarter(stored.HypervisorType)
	if err != nil {
		return nil, false, fmt.Errorf("get vm starter: %w", err)
	}

	now := time.Now().UTC()
	forkMeta := cloneStoredMetadataWithoutPendingStandbyCompression(meta.StoredMetadata)
	forkMeta.Id = forkID
	forkMeta.Name = req.Name
	forkMeta.CreatedAt = now
	forkMeta.StartedAt = nil
	forkMeta.StoppedAt = nil
	forkMeta.HypervisorPID = nil
	forkMeta.SocketPath = m.paths.InstanceSocket(forkID, starter.SocketName())
	forkMeta.DataDir = dstDir
	forkMeta.VsockSocket = m.paths.InstanceSocket(forkID, hypervisor.VsockSocketNameForType(forkMeta.HypervisorType))
	forkMeta.ExitCode = nil
	forkMeta.ExitMessage = ""
	forkMeta.RestartStatus = restartpolicy.Status{}
	forkMeta.FirecrackerUFFDSessionID = ""
	forkMeta.FirecrackerUFFDPagerVersion = ""
	forkMeta.FirecrackerUseUFFDOnNextRestore = useFirecrackerUFFDOnNextRestore(forkMeta.HypervisorType, source.State == StateStandby, targetState)
	if source.State != StateStandby {
		forkMeta.FirecrackerSnapshotCacheKey = ""
	}
	// Forks are new instances; phase accounting must not inherit the source's
	// cumulative durations. The first transition into the fork's runtime
	// phase (Standby for snapshot forks, Stopped for stopped forks) will be
	// recorded by the appropriate operation when the fork is acted on.
	forkMeta.Phases.Reset()
	switch source.State {
	case StateStandby:
		forkMeta.Phases.Record(phasetracking.PhaseStandby, now)
	case StateStopped:
		forkMeta.Phases.Record(phasetracking.PhaseStopped, now)
	}

	// Keep the original CID for snapshot-based forks. Windows' restored VioSock
	// driver retains the CID captured in guest memory until the next cold boot.
	if source.State == StateStandby {
		forkMeta.VsockCID = stored.VsockCID
	} else {
		forkMeta.VsockCID = generateVsockCID(forkID)
	}

	if err := m.prepareWindowsForkIdentity(&forkMeta); err != nil {
		return nil, false, err
	}

	if forkMeta.NetworkEnabled {
		// Clear inherited network identity. For stopped instances this is regenerated on start,
		// and for standby instances restore allocates if identity is empty.
		forkMeta.IP = ""
		forkMeta.MAC = ""
	}

	restoreNeedsSourceLock := false
	if source.State == StateStandby {
		snapshotConfigPath := m.paths.InstanceSnapshotConfig(forkID)
		netCfg := (*hypervisor.ForkNetworkConfig)(nil)
		if forkMeta.NetworkEnabled {
			netCfg = &hypervisor.ForkNetworkConfig{TAPDevice: network.GenerateTAPName(forkID)}
		}
		prepareResult, err := prepareForkWithAliasReadLock(ctx, starter, hypervisor.ForkPrepareRequest{
			SnapshotConfigPath: snapshotConfigPath,
			SourceDataDir:      stored.DataDir,
			TargetDataDir:      forkMeta.DataDir,
			VsockCID:           forkMeta.VsockCID,
			VsockSocket:        forkMeta.VsockSocket,
			SerialLogPath:      m.paths.InstanceAppLog(forkID),
			Network:            netCfg,
		})
		if err != nil {
			if errors.Is(err, hypervisor.ErrNotSupported) {
				return nil, false, fmt.Errorf("%w: fork is not supported for hypervisor %s", ErrNotSupported, stored.HypervisorType)
			}
			return nil, false, fmt.Errorf("prepare fork snapshot state: %w", err)
		}
		if prepareResult.RequiresSnapshotSourceAlias {
			log.WarnContext(ctx, "fork restore requires snapshot source alias fallback",
				"hypervisor", stored.HypervisorType,
				"source_instance_id", id,
				"fork_instance_id", forkID,
				"source_data_dir", stored.DataDir,
				"target_data_dir", forkMeta.DataDir)
		}
		restoreNeedsSourceLock = prepareResult.RequiresSnapshotSourceAlias
	}

	newMeta := &metadata{StoredMetadata: forkMeta}
	if err := m.saveForkMetadata(ctx, newMeta); err != nil {
		return nil, false, err
	}

	cu.Release()
	forked := m.toInstance(ctx, newMeta)
	log.InfoContext(ctx, "instance forked successfully",
		"source_instance_id", id,
		"fork_instance_id", forked.Id,
		"fork_name", forked.Name,
		"state", forked.State)
	return &forked, restoreNeedsSourceLock, nil
}

func (m *manager) saveForkMetadata(ctx context.Context, meta *metadata) error {
	ctx, span := m.tracerOrDefault().Start(ctx, "instances.fork_metadata.save",
		trace.WithAttributes(attribute.String("operation", "save_fork_metadata")),
	)
	var retErr error
	defer func() { finishInstancesSpan(span, retErr) }()

	lockCtx, lockWaitSpan := m.tracerOrDefault().Start(ctx, "instances.fork_metadata.lock_wait",
		trace.WithAttributes(attribute.String("operation", "fork_metadata_lock_wait")),
	)
	m.forkMetadataMu.Lock()
	finishInstancesSpan(lockWaitSpan, nil)

	holdCtx, lockHoldSpan := m.tracerOrDefault().Start(lockCtx, "instances.fork_metadata.lock_hold",
		trace.WithAttributes(attribute.String("operation", "fork_metadata_lock_hold")),
	)
	defer m.forkMetadataMu.Unlock()
	defer func() { finishInstancesSpan(lockHoldSpan, retErr) }()

	// Earlier name checks are advisory so callers can fail before doing fork
	// work when possible. This is the serialized admission point for fork
	// metadata, so concurrent forks re-check names immediately before save.
	name := meta.Name
	existsByMetadata, err := m.instanceNameExists(holdCtx, name, "fork_admission")
	if err != nil {
		retErr = fmt.Errorf("check instance name availability: %w", err)
		return retErr
	}
	if existsByMetadata {
		retErr = fmt.Errorf("%w: instance name '%s' already exists", ErrAlreadyExists, name)
		return retErr
	}
	if meta.NetworkEnabled {
		exists, err := m.networkManager.NameExists(holdCtx, name, "")
		if err != nil {
			retErr = fmt.Errorf("check instance name availability: %w", err)
			return retErr
		}
		if exists {
			retErr = fmt.Errorf("%w: instance name '%s' already exists in network", ErrAlreadyExists, name)
			return retErr
		}
	}
	if err := m.saveMetadata(meta); err != nil {
		retErr = fmt.Errorf("save fork metadata: %w", err)
		return retErr
	}
	return nil
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

	returnWithReadiness := func(inst *Instance, err error, guestReady bool) (*Instance, error) {
		if err != nil {
			return nil, err
		}
		if forkReturnNeedsGuestAgentReady(inst, guestReady) {
			if err := ensureGuestAgentReadyForForkPhase(ctx, &inst.StoredMetadata, "before returning running fork instance"); err != nil {
				return nil, fmt.Errorf("wait for forked guest agent readiness: %w", err)
			}
		}
		return inst, nil
	}

	current, err := m.getInstance(ctx, forkID)
	if err != nil {
		return nil, err
	}
	if current.State == target || (target == StateRunning && current.State == StateInitializing) {
		return returnWithReadiness(current, nil, false)
	}

	switch current.State {
	case StateStopped:
		switch target {
		case StateRunning:
			inst, err := m.startInstance(ctx, forkID, StartInstanceRequest{})
			return returnWithReadiness(inst, err, false)
		case StateStandby:
			if _, err := m.startInstance(ctx, forkID, StartInstanceRequest{}); err != nil {
				return nil, fmt.Errorf("start forked instance for standby transition: %w", err)
			}
			inst, err := m.standbyInstance(ctx, forkID, StandbyInstanceRequest{}, false)
			return returnWithReadiness(inst, err, false)
		}
	case StateStandby:
		switch target {
		case StateRunning:
			inst, err := m.restoreInstance(ctx, forkID)
			return returnWithReadiness(inst, err, current.NetworkEnabled && !current.SkipGuestAgent)
		case StateStopped:
			if err := os.RemoveAll(m.paths.InstanceSnapshotLatest(forkID)); err != nil {
				return nil, fmt.Errorf("remove fork snapshot: %w", err)
			}
			meta, err := m.loadMetadata(forkID)
			if err != nil {
				return nil, fmt.Errorf("load stopped fork metadata: %w", err)
			}
			meta.StoredMetadata.VsockCID = generateVsockCID(forkID)
			meta.StoredMetadata.FirecrackerSnapshotCacheKey = ""
			clearFirecrackerUFFDRestoreState(&meta.StoredMetadata)
			if err := m.saveMetadata(meta); err != nil {
				return nil, fmt.Errorf("save stopped fork metadata: %w", err)
			}
			inst, err := m.getInstance(ctx, forkID)
			return returnWithReadiness(inst, err, false)
		}
	case StateRunning:
		switch target {
		case StateStandby:
			inst, err := m.standbyInstance(ctx, forkID, StandbyInstanceRequest{}, false)
			return returnWithReadiness(inst, err, false)
		case StateStopped:
			inst, err := m.stopInstance(ctx, forkID)
			return returnWithReadiness(inst, err, false)
		}
	}

	return nil, fmt.Errorf("%w: cannot transition forked instance from %s to %s", ErrInvalidState, current.State, target)
}

func forkTargetStateAlreadyApplied(inst *Instance, target State) bool {
	if inst == nil {
		return false
	}
	return inst.State == target || (target == StateRunning && inst.State == StateInitializing)
}

func forkReturnNeedsGuestAgentReady(inst *Instance, guestReady bool) bool {
	return inst != nil && (inst.State == StateRunning || inst.State == StateInitializing) && !guestReady
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

func cloneStoredMetadata(src StoredMetadata) StoredMetadata {
	dst := src
	dst.PendingStandbyCompression = clonePendingStandbyCompression(src.PendingStandbyCompression)

	if src.Env != nil {
		dst.Env = make(map[string]string, len(src.Env))
		for k, v := range src.Env {
			dst.Env[k] = v
		}
	}
	if src.NetworkEgress != nil {
		dst.NetworkEgress = cloneNetworkEgressPolicy(src.NetworkEgress)
	}
	if src.AutoStandby != nil {
		dst.AutoStandby = cloneAutoStandbyPolicy(src.AutoStandby)
	}
	if src.HealthCheck != nil {
		dst.HealthCheck = cloneHealthCheckPolicy(src.HealthCheck)
	}
	if src.RestartPolicy != nil {
		dst.RestartPolicy = cloneRestartPolicy(src.RestartPolicy)
	}
	if src.SnapshotPolicy != nil {
		dst.SnapshotPolicy = cloneSnapshotPolicy(src.SnapshotPolicy)
	}
	if src.Credentials != nil {
		dst.Credentials = cloneCredentialPolicies(src.Credentials)
	}
	if src.Tags != nil {
		dst.Tags = make(map[string]string, len(src.Tags))
		for k, v := range src.Tags {
			dst.Tags[k] = v
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
	if src.ProgramStartedAt != nil {
		programStartedAt := *src.ProgramStartedAt
		dst.ProgramStartedAt = &programStartedAt
	}
	if src.GuestAgentReadyAt != nil {
		guestAgentReadyAt := *src.GuestAgentReadyAt
		dst.GuestAgentReadyAt = &guestAgentReadyAt
	}
	if src.ExitCode != nil {
		exitCode := *src.ExitCode
		dst.ExitCode = &exitCode
	}
	dst.Phases = src.Phases.Clone()

	return dst
}

func cloneStoredMetadataWithoutPendingStandbyCompression(src StoredMetadata) StoredMetadata {
	dst := cloneStoredMetadata(src)
	dst.PendingStandbyCompression = nil
	return dst
}
