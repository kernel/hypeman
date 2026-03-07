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
	snapshotstore "github.com/kernel/hypeman/lib/snapshot"
	"github.com/nrednav/cuid2"
	"gvisor.dev/gvisor/pkg/cleanup"
)

type snapshotRecord struct {
	Snapshot       Snapshot
	StoredMetadata StoredMetadata
}

func (m *manager) listSnapshots(ctx context.Context, filter *ListSnapshotsFilter) ([]Snapshot, error) {
	_ = ctx
	snapshots, err := m.snapshotStore().List(filter)
	if err != nil {
		return nil, fmt.Errorf("list snapshots: %w", err)
	}
	return snapshots, nil
}

func (m *manager) getSnapshot(ctx context.Context, snapshotID string) (*Snapshot, error) {
	_ = ctx
	snapshot, err := m.snapshotStore().Get(snapshotID)
	if err != nil {
		if errors.Is(err, snapshotstore.ErrNotFound) {
			return nil, ErrSnapshotNotFound
		}
		return nil, err
	}
	return snapshot, nil
}

func (m *manager) createSnapshot(ctx context.Context, id string, req CreateSnapshotRequest) (*Snapshot, error) {
	log := logger.FromContext(ctx)
	log.InfoContext(ctx, "creating snapshot", "instance_id", id, "kind", req.Kind, "name", req.Name)

	if err := validateCreateSnapshotRequest(req); err != nil {
		return nil, err
	}

	meta, err := m.loadMetadata(id)
	if err != nil {
		return nil, err
	}
	inst := m.toInstance(ctx, meta)
	stored := &meta.StoredMetadata

	if err := validateForkVolumeSafety(stored.Volumes); err != nil {
		return nil, fmt.Errorf("%w: snapshot requires readonly volume attachments: %v", ErrNotSupported, err)
	}
	if err := m.ensureSnapshotNameAvailable(stored.Id, req.Name); err != nil {
		return nil, err
	}

	snapshotID := cuid2.Generate()
	if _, err := m.loadSnapshotRecord(snapshotID); err == nil {
		return nil, fmt.Errorf("%w: generated snapshot id already exists", ErrAlreadyExists)
	} else if !errors.Is(err, ErrSnapshotNotFound) {
		return nil, err
	}

	snapshotDir := m.paths.SnapshotDir(snapshotID)
	snapshotGuestDir := m.paths.SnapshotGuestDir(snapshotID)
	cu := cleanup.Make(func() {
		_ = os.RemoveAll(snapshotDir)
	})
	defer cu.Clean()

	if err := os.MkdirAll(m.paths.SnapshotStoreDir(), 0755); err != nil {
		return nil, fmt.Errorf("create snapshot store dir: %w", err)
	}

	switch req.Kind {
	case SnapshotKindStandby:
		restoreSource := false
		switch inst.State {
		case StateRunning:
			if err := ensureGuestAgentReadyForForkPhase(ctx, &inst.StoredMetadata, "before running snapshot"); err != nil {
				return nil, err
			}
			if _, err := m.standbyInstance(ctx, id); err != nil {
				return nil, fmt.Errorf("standby source instance: %w", err)
			}
			restoreSource = true
		case StateStandby:
			// already ready to copy
		default:
			return nil, fmt.Errorf("%w: standby snapshot requires source in %s or %s, got %s", ErrInvalidState, StateRunning, StateStandby, inst.State)
		}

		copyErr := m.copySnapshotPayload(id, snapshotGuestDir)
		if copyErr == nil {
			meta, copyErr = m.loadMetadata(id)
		}

		if restoreSource {
			_, restoreErr := m.restoreInstance(ctx, id)
			if restoreErr != nil {
				if copyErr != nil {
					return nil, fmt.Errorf("snapshot copy failed: %v; additionally failed to restore source: %w", copyErr, restoreErr)
				}
				return nil, fmt.Errorf("restore source after snapshot: %w", restoreErr)
			}
		}

		if copyErr != nil {
			return nil, copyErr
		}

		rec := &snapshotRecord{
			Snapshot: Snapshot{
				Id:               snapshotID,
				Name:             req.Name,
				Kind:             req.Kind,
				SourceInstanceID: stored.Id,
				SourceName:       stored.Name,
				SourceHypervisor: stored.HypervisorType,
				CreatedAt:        time.Now(),
			},
			StoredMetadata: cloneStoredMetadataForFork(meta.StoredMetadata),
		}
		sizeBytes, err := snapshotstore.DirectoryFileSize(snapshotGuestDir)
		if err != nil {
			return nil, err
		}
		rec.Snapshot.SizeBytes = sizeBytes
		if err := m.saveSnapshotRecord(rec); err != nil {
			return nil, err
		}
		cu.Release()
		log.InfoContext(ctx, "snapshot created", "instance_id", id, "snapshot_id", snapshotID, "kind", req.Kind)
		return &rec.Snapshot, nil

	case SnapshotKindStopped:
		if inst.State != StateStopped {
			return nil, fmt.Errorf("%w: stopped snapshot requires source in %s, got %s", ErrInvalidState, StateStopped, inst.State)
		}
		if err := m.copySnapshotPayload(id, snapshotGuestDir); err != nil {
			return nil, err
		}
		rec := &snapshotRecord{
			Snapshot: Snapshot{
				Id:               snapshotID,
				Name:             req.Name,
				Kind:             req.Kind,
				SourceInstanceID: stored.Id,
				SourceName:       stored.Name,
				SourceHypervisor: stored.HypervisorType,
				CreatedAt:        time.Now(),
			},
			StoredMetadata: cloneStoredMetadataForFork(meta.StoredMetadata),
		}
		sizeBytes, err := snapshotstore.DirectoryFileSize(snapshotGuestDir)
		if err != nil {
			return nil, err
		}
		rec.Snapshot.SizeBytes = sizeBytes
		if err := m.saveSnapshotRecord(rec); err != nil {
			return nil, err
		}
		cu.Release()
		log.InfoContext(ctx, "snapshot created", "instance_id", id, "snapshot_id", snapshotID, "kind", req.Kind)
		return &rec.Snapshot, nil

	default:
		return nil, fmt.Errorf("%w: unsupported snapshot kind %q", ErrInvalidRequest, req.Kind)
	}
}

func (m *manager) deleteSnapshot(ctx context.Context, snapshotID string) error {
	_ = ctx
	if err := m.snapshotStore().Delete(snapshotID); err != nil {
		if errors.Is(err, snapshotstore.ErrNotFound) {
			return ErrSnapshotNotFound
		}
		return err
	}
	return nil
}

func (m *manager) restoreSnapshot(ctx context.Context, id string, snapshotID string, req RestoreSnapshotRequest) (*Instance, error) {
	log := logger.FromContext(ctx)
	rec, err := m.loadSnapshotRecord(snapshotID)
	if err != nil {
		return nil, err
	}
	if rec.Snapshot.SourceInstanceID != id {
		return nil, fmt.Errorf("%w: snapshot %s belongs to instance %s", ErrInvalidRequest, snapshotID, rec.Snapshot.SourceInstanceID)
	}

	sourceMeta, err := m.loadMetadata(id)
	if err != nil {
		return nil, err
	}
	sourceInst := m.toInstance(ctx, sourceMeta)
	if sourceInst.State == StateRunning {
		return nil, fmt.Errorf("%w: cannot restore snapshot while source is %s", ErrInvalidState, sourceInst.State)
	}

	targetState, err := resolveSnapshotTargetState(rec.Snapshot.Kind, req.TargetState)
	if err != nil {
		return nil, err
	}
	targetHypervisor, err := m.resolveSnapshotTargetHypervisor(rec, req.TargetHypervisor)
	if err != nil {
		return nil, err
	}

	if err := m.replaceInstanceWithSnapshotPayload(snapshotID, id); err != nil {
		return nil, err
	}

	restored := cloneStoredMetadataForFork(rec.StoredMetadata)
	restored.Id = sourceMeta.Id
	restored.Name = sourceMeta.Name
	restored.DataDir = m.paths.InstanceDir(id)
	restored.HypervisorPID = nil
	restored.StartedAt = nil
	restored.StoppedAt = nil
	restored.ExitCode = nil
	restored.ExitMessage = ""
	restored.HypervisorType = targetHypervisor

	starter, err := m.getVMStarter(targetHypervisor)
	if err != nil {
		return nil, fmt.Errorf("get vm starter: %w", err)
	}
	hvVersion, err := starter.GetVersion(m.paths)
	if err != nil {
		log.WarnContext(ctx, "failed to get hypervisor version", "hypervisor", targetHypervisor, "error", err)
		hvVersion = "unknown"
	}
	restored.HypervisorVersion = hvVersion
	restored.SocketPath = m.paths.InstanceSocket(id, starter.SocketName())
	restored.VsockSocket = m.paths.InstanceSocket(id, hypervisor.VsockSocketNameForType(targetHypervisor))
	if rec.Snapshot.Kind == SnapshotKindStopped {
		restored.VsockCID = generateVsockCID(id)
	}

	if err := m.saveMetadata(&metadata{StoredMetadata: restored}); err != nil {
		return nil, fmt.Errorf("save restored metadata: %w", err)
	}

	switch rec.Snapshot.Kind {
	case SnapshotKindStandby:
		switch targetState {
		case StateStandby:
			return m.getInstance(ctx, id)
		case StateStopped:
			if err := os.RemoveAll(m.paths.InstanceSnapshotLatest(id)); err != nil {
				return nil, fmt.Errorf("remove instance snapshot: %w", err)
			}
			return m.getInstance(ctx, id)
		case StateRunning:
			inst, err := m.restoreInstance(ctx, id)
			if err != nil {
				return nil, err
			}
			if err := ensureGuestAgentReadyForForkPhase(ctx, &inst.StoredMetadata, "before returning running snapshot restore instance"); err != nil {
				return nil, fmt.Errorf("wait for snapshot restore guest agent readiness: %w", err)
			}
			return inst, nil
		}
	case SnapshotKindStopped:
		switch targetState {
		case StateStopped:
			_ = os.RemoveAll(m.paths.InstanceSnapshotLatest(id))
			return m.getInstance(ctx, id)
		case StateRunning:
			inst, err := m.startInstance(ctx, id, StartInstanceRequest{})
			if err != nil {
				return nil, err
			}
			if err := ensureGuestAgentReadyForForkPhase(ctx, &inst.StoredMetadata, "before returning running snapshot restore instance"); err != nil {
				return nil, fmt.Errorf("wait for snapshot restore guest agent readiness: %w", err)
			}
			return inst, nil
		}
	}

	return nil, fmt.Errorf("%w: unsupported restore target state %s for snapshot kind %s", ErrInvalidRequest, targetState, rec.Snapshot.Kind)
}

func (m *manager) forkSnapshot(ctx context.Context, snapshotID string, req ForkSnapshotRequest) (*Instance, error) {
	if err := validateForkSnapshotRequest(req); err != nil {
		return nil, err
	}

	rec, err := m.loadSnapshotRecord(snapshotID)
	if err != nil {
		return nil, err
	}
	if err := validateForkVolumeSafety(rec.StoredMetadata.Volumes); err != nil {
		return nil, fmt.Errorf("%w: snapshot requires readonly volume attachments: %v", ErrNotSupported, err)
	}

	if err := m.ensureInstanceNameAvailableForSnapshotFork(ctx, req.Name, rec.StoredMetadata.NetworkEnabled); err != nil {
		return nil, err
	}

	targetState, err := resolveSnapshotTargetState(rec.Snapshot.Kind, req.TargetState)
	if err != nil {
		return nil, err
	}
	targetHypervisor, err := m.resolveSnapshotTargetHypervisor(rec, req.TargetHypervisor)
	if err != nil {
		return nil, err
	}

	forkID := cuid2.Generate()
	if _, err := m.loadMetadata(forkID); err == nil {
		return nil, fmt.Errorf("%w: generated fork id already exists", ErrAlreadyExists)
	} else if !errors.Is(err, ErrNotFound) {
		return nil, err
	}

	dstDir := m.paths.InstanceDir(forkID)
	cu := cleanup.Make(func() {
		_ = os.RemoveAll(dstDir)
	})
	defer cu.Clean()

	if err := forkvm.CopyGuestDirectory(m.paths.SnapshotGuestDir(snapshotID), dstDir); err != nil {
		if errors.Is(err, forkvm.ErrSparseCopyUnsupported) {
			return nil, fmt.Errorf("fork from snapshot requires sparse-capable filesystem (SEEK_DATA/SEEK_HOLE unsupported): %w", err)
		}
		return nil, fmt.Errorf("clone snapshot payload: %w", err)
	}

	starter, err := m.getVMStarter(targetHypervisor)
	if err != nil {
		return nil, fmt.Errorf("get vm starter: %w", err)
	}
	hvVersion, err := starter.GetVersion(m.paths)
	if err != nil {
		hvVersion = "unknown"
	}

	now := time.Now()
	forkMeta := cloneStoredMetadataForFork(rec.StoredMetadata)
	forkMeta.Id = forkID
	forkMeta.Name = req.Name
	forkMeta.CreatedAt = now
	forkMeta.StartedAt = nil
	forkMeta.StoppedAt = nil
	forkMeta.HypervisorPID = nil
	forkMeta.DataDir = dstDir
	forkMeta.HypervisorType = targetHypervisor
	forkMeta.HypervisorVersion = hvVersion
	forkMeta.SocketPath = m.paths.InstanceSocket(forkID, starter.SocketName())
	forkMeta.VsockSocket = m.paths.InstanceSocket(forkID, hypervisor.VsockSocketNameForType(targetHypervisor))
	forkMeta.ExitCode = nil
	forkMeta.ExitMessage = ""
	if rec.Snapshot.Kind == SnapshotKindStandby {
		forkMeta.VsockCID = rec.StoredMetadata.VsockCID
	} else {
		forkMeta.VsockCID = generateVsockCID(forkID)
	}
	if forkMeta.NetworkEnabled {
		forkMeta.IP = ""
		forkMeta.MAC = ""
	}

	if rec.Snapshot.Kind == SnapshotKindStandby {
		netCfg := (*hypervisor.ForkNetworkConfig)(nil)
		if forkMeta.NetworkEnabled {
			netCfg = &hypervisor.ForkNetworkConfig{TAPDevice: network.GenerateTAPName(forkID)}
		}
		if _, err := starter.PrepareFork(ctx, hypervisor.ForkPrepareRequest{
			SnapshotConfigPath: m.paths.InstanceSnapshotConfig(forkID),
			SourceDataDir:      rec.StoredMetadata.DataDir,
			TargetDataDir:      forkMeta.DataDir,
			VsockCID:           forkMeta.VsockCID,
			VsockSocket:        forkMeta.VsockSocket,
			SerialLogPath:      m.paths.InstanceAppLog(forkID),
			Network:            netCfg,
		}); err != nil {
			if errors.Is(err, hypervisor.ErrNotSupported) {
				return nil, fmt.Errorf("%w: snapshot fork is not supported for hypervisor %s", ErrNotSupported, targetHypervisor)
			}
			return nil, fmt.Errorf("prepare snapshot fork state: %w", err)
		}
	}

	if err := m.saveMetadata(&metadata{StoredMetadata: forkMeta}); err != nil {
		return nil, fmt.Errorf("save fork metadata: %w", err)
	}

	cu.Release()
	inst, err := m.applyForkTargetState(ctx, forkID, targetState)
	if err != nil {
		if cleanupErr := m.cleanupForkInstanceOnError(ctx, forkID); cleanupErr != nil {
			return nil, fmt.Errorf("apply snapshot fork target state: %w; additionally failed to cleanup forked instance %s: %v", err, forkID, cleanupErr)
		}
		return nil, fmt.Errorf("apply snapshot fork target state: %w", err)
	}
	return inst, nil
}

func (m *manager) copySnapshotPayload(sourceInstanceID, snapshotGuestDir string) error {
	if err := forkvm.CopyGuestDirectory(m.paths.InstanceDir(sourceInstanceID), snapshotGuestDir); err != nil {
		if errors.Is(err, forkvm.ErrSparseCopyUnsupported) {
			return fmt.Errorf("snapshot requires sparse-capable filesystem (SEEK_DATA/SEEK_HOLE unsupported): %w", err)
		}
		return fmt.Errorf("copy guest directory into snapshot: %w", err)
	}
	return nil
}

func (m *manager) replaceInstanceWithSnapshotPayload(snapshotID, instanceID string) error {
	instanceDir := m.paths.InstanceDir(instanceID)
	if err := os.RemoveAll(instanceDir); err != nil {
		return fmt.Errorf("clear instance directory: %w", err)
	}
	if err := forkvm.CopyGuestDirectory(m.paths.SnapshotGuestDir(snapshotID), instanceDir); err != nil {
		if errors.Is(err, forkvm.ErrSparseCopyUnsupported) {
			return fmt.Errorf("restore requires sparse-capable filesystem (SEEK_DATA/SEEK_HOLE unsupported): %w", err)
		}
		return fmt.Errorf("restore snapshot payload: %w", err)
	}
	return nil
}

func (m *manager) resolveSnapshotTargetHypervisor(rec *snapshotRecord, requested hypervisor.Type) (hypervisor.Type, error) {
	if requested == "" {
		return rec.StoredMetadata.HypervisorType, nil
	}
	if rec.Snapshot.Kind == SnapshotKindStandby {
		return "", fmt.Errorf("%w: target_hypervisor is only allowed for stopped snapshots", ErrInvalidRequest)
	}
	if _, err := m.getVMStarter(requested); err != nil {
		return "", fmt.Errorf("%w: unsupported target hypervisor %q", ErrInvalidRequest, requested)
	}
	return requested, nil
}

func resolveSnapshotTargetState(kind SnapshotKind, requested State) (State, error) {
	resolved, err := snapshotstore.ResolveTargetState(kind, string(requested))
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrInvalidRequest, err)
	}
	return State(resolved), nil
}

func validateCreateSnapshotRequest(req CreateSnapshotRequest) error {
	if req.Kind != SnapshotKindStandby && req.Kind != SnapshotKindStopped {
		return fmt.Errorf("%w: kind must be one of %s, %s", ErrInvalidRequest, SnapshotKindStandby, SnapshotKindStopped)
	}
	if req.Name != "" {
		if err := validateInstanceName(req.Name); err != nil {
			return fmt.Errorf("%w: %v", ErrInvalidRequest, err)
		}
	}
	return nil
}

func validateForkSnapshotRequest(req ForkSnapshotRequest) error {
	if err := validateInstanceName(req.Name); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidRequest, err)
	}
	if req.TargetState != "" && req.TargetState != StateStopped && req.TargetState != StateStandby && req.TargetState != StateRunning {
		return fmt.Errorf("%w: invalid target_state %q", ErrInvalidRequest, req.TargetState)
	}
	return nil
}

func (m *manager) snapshotStore() *snapshotstore.Store {
	return snapshotstore.NewStore(m.paths)
}

func (m *manager) ensureSnapshotNameAvailable(sourceInstanceID, snapshotName string) error {
	if err := m.snapshotStore().EnsureNameAvailable(sourceInstanceID, snapshotName); err != nil {
		if errors.Is(err, snapshotstore.ErrNameExists) {
			return fmt.Errorf("%w: %v", ErrAlreadyExists, err)
		}
		return err
	}
	return nil
}

func (m *manager) ensureInstanceNameAvailableForSnapshotFork(ctx context.Context, name string, networkEnabled bool) error {
	existsByMetadata, err := m.instanceNameExists(name)
	if err != nil {
		return fmt.Errorf("check instance name availability: %w", err)
	}
	if existsByMetadata {
		return fmt.Errorf("%w: instance name '%s' already exists", ErrAlreadyExists, name)
	}
	if networkEnabled {
		exists, err := m.networkManager.NameExists(ctx, name, "")
		if err != nil {
			return fmt.Errorf("check instance name availability: %w", err)
		}
		if exists {
			return fmt.Errorf("%w: instance name '%s' already exists in network", ErrAlreadyExists, name)
		}
	}
	return nil
}

func (m *manager) saveSnapshotRecord(rec *snapshotRecord) error {
	if err := snapshotstore.SaveTypedRecord(m.snapshotStore(), &snapshotstore.TypedRecord[StoredMetadata]{
		Snapshot:       rec.Snapshot,
		StoredMetadata: rec.StoredMetadata,
	}); err != nil {
		return err
	}
	return nil
}

func (m *manager) loadSnapshotRecord(snapshotID string) (*snapshotRecord, error) {
	record, err := snapshotstore.LoadTypedRecord[StoredMetadata](m.snapshotStore(), snapshotID)
	if err != nil {
		if errors.Is(err, snapshotstore.ErrNotFound) {
			return nil, ErrSnapshotNotFound
		}
		return nil, err
	}
	return &snapshotRecord{
		Snapshot:       record.Snapshot,
		StoredMetadata: record.StoredMetadata,
	}, nil
}

func (m *manager) listSnapshotRecords() ([]snapshotRecord, error) {
	storedRecords, err := snapshotstore.ListTypedRecords[StoredMetadata](m.snapshotStore())
	if err != nil {
		return nil, err
	}
	records := make([]snapshotRecord, 0, len(storedRecords))
	for _, stored := range storedRecords {
		records = append(records, snapshotRecord{
			Snapshot:       stored.Snapshot,
			StoredMetadata: stored.StoredMetadata,
		})
	}
	return records, nil
}
