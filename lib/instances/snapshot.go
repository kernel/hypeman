package instances

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/kernel/hypeman/lib/hypervisor"
	"github.com/kernel/hypeman/lib/logger"
	"github.com/kernel/hypeman/lib/network"
	snapshotstore "github.com/kernel/hypeman/lib/snapshot"
	"github.com/nrednav/cuid2"
	"gvisor.dev/gvisor/pkg/cleanup"
)

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

	snap, err := snapshotstore.RunCreateWorkflow(ctx, snapshotstore.CreateWorkflowDeps{
		ValidateRequest: func(in snapshotstore.CreateWorkflowRequest) error {
			return snapshotstore.ValidateCreateRequest(in.Kind, in.Name, validateInstanceName)
		},
		LoadSource: func(ctx context.Context, sourceID string) (*snapshotstore.SourceInfo, error) {
			meta, err := m.loadMetadata(sourceID)
			if err != nil {
				return nil, err
			}
			if err := validateForkVolumeSafety(meta.StoredMetadata.Volumes); err != nil {
				return nil, fmt.Errorf("%w: snapshot requires readonly volume attachments: %v", ErrNotSupported, err)
			}
			inst := m.toInstance(ctx, meta)
			return &snapshotstore.SourceInfo{State: string(inst.State)}, nil
		},
		EnsureSnapshotNameAvailable: func(ctx context.Context, sourceID, name string) error {
			return m.ensureSnapshotNameAvailable(sourceID, name)
		},
		GenerateSnapshotID: func() string {
			return cuid2.Generate()
		},
		SnapshotIDExists: func(ctx context.Context, snapshotID string) (bool, error) {
			_, err := m.snapshotStore().Get(snapshotID)
			if err == nil {
				return true, nil
			}
			if errors.Is(err, snapshotstore.ErrNotFound) {
				return false, nil
			}
			return false, err
		},
		EnsureGuestAgentReady: func(ctx context.Context, sourceID, phase string) error {
			return m.ensureGuestAgentReadyByInstanceID(ctx, sourceID, phase)
		},
		StandbySource: func(ctx context.Context, sourceID string) error {
			_, err := m.standbyInstance(ctx, sourceID)
			return err
		},
		RestoreSource: func(ctx context.Context, sourceID string) error {
			_, err := m.restoreInstance(ctx, sourceID)
			return err
		},
		CopySourceToSnapshot: func(ctx context.Context, sourceID, snapshotID string) error {
			_ = ctx
			return snapshotstore.CopyPayload(m.paths.InstanceDir(sourceID), m.paths.SnapshotGuestDir(snapshotID), "copy guest directory into snapshot")
		},
		BuildRecord: func(ctx context.Context, in snapshotstore.CreateWorkflowRequest, snapshotID string) (*snapshotstore.Record, error) {
			return m.buildSnapshotStoreRecord(ctx, in.SourceInstanceID, CreateSnapshotRequest{
				Kind: in.Kind,
				Name: in.Name,
			}, snapshotID)
		},
		SaveRecord: func(ctx context.Context, record *snapshotstore.Record) error {
			_ = ctx
			return m.snapshotStore().SaveRecord(record)
		},
		EnsureSnapshotStoreDir: func(ctx context.Context) error {
			_ = ctx
			return snapshotstore.EnsureDir(m.paths.SnapshotStoreDir())
		},
		RemoveSnapshotDir: func(ctx context.Context, snapshotID string) error {
			_ = ctx
			return os.RemoveAll(m.paths.SnapshotDir(snapshotID))
		},
	}, snapshotstore.CreateWorkflowRequest{
		SourceInstanceID: id,
		Kind:             req.Kind,
		Name:             req.Name,
	})
	if err != nil {
		return nil, m.mapWorkflowError(err)
	}

	log.InfoContext(ctx, "snapshot created", "instance_id", id, "snapshot_id", snap.Id, "kind", snap.Kind)
	return snap, nil
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
	instanceID, err := snapshotstore.RunRestoreWorkflow(ctx, snapshotstore.RestoreWorkflowDeps{
		LoadRecord: func(ctx context.Context, snapshotID string) (*snapshotstore.Record, error) {
			_ = ctx
			record, err := m.snapshotStore().LoadRecord(snapshotID)
			if err != nil {
				if errors.Is(err, snapshotstore.ErrNotFound) {
					return nil, ErrSnapshotNotFound
				}
				return nil, err
			}
			return record, nil
		},
		LoadSource: func(ctx context.Context, sourceID string) (*snapshotstore.SourceInfo, error) {
			meta, err := m.loadMetadata(sourceID)
			if err != nil {
				return nil, err
			}
			inst := m.toInstance(ctx, meta)
			return &snapshotstore.SourceInfo{State: string(inst.State)}, nil
		},
		ResolveTargetHypervisor: func(ctx context.Context, record *snapshotstore.Record, requested string) (string, error) {
			_ = ctx
			stored, err := snapshotstore.DecodeRecordMetadata[StoredMetadata](record)
			if err != nil {
				return "", err
			}
			hv, err := m.resolveSnapshotTargetHypervisor(record.Snapshot.Kind, stored.HypervisorType, hypervisor.Type(requested))
			if err != nil {
				return "", err
			}
			return string(hv), nil
		},
		ReplaceInstanceFromRecord: func(ctx context.Context, snapshotID, sourceID string) error {
			_ = ctx
			return snapshotstore.ReplacePayload(m.paths.SnapshotGuestDir(snapshotID), m.paths.InstanceDir(sourceID), "restore snapshot payload")
		},
		PrepareRestoreMetadata: func(ctx context.Context, sourceID string, record *snapshotstore.Record, targetHypervisor string) error {
			stored, err := snapshotstore.DecodeRecordMetadata[StoredMetadata](record)
			if err != nil {
				return err
			}
			return m.prepareRestoreMetadata(ctx, sourceID, record.Snapshot.Kind, stored, hypervisor.Type(targetHypervisor))
		},
		RemoveInstanceSnapshot: func(ctx context.Context, sourceID string) error {
			_ = ctx
			return os.RemoveAll(m.paths.InstanceSnapshotLatest(sourceID))
		},
		RestoreToRunning: func(ctx context.Context, sourceID string) error {
			inst, err := m.restoreInstance(ctx, sourceID)
			if err != nil {
				return err
			}
			if err := ensureGuestAgentReadyForForkPhase(ctx, &inst.StoredMetadata, "before returning running snapshot restore instance"); err != nil {
				return fmt.Errorf("wait for snapshot restore guest agent readiness: %w", err)
			}
			return nil
		},
		StartToRunning: func(ctx context.Context, sourceID string) error {
			inst, err := m.startInstance(ctx, sourceID, StartInstanceRequest{})
			if err != nil {
				return err
			}
			if err := ensureGuestAgentReadyForForkPhase(ctx, &inst.StoredMetadata, "before returning running snapshot restore instance"); err != nil {
				return fmt.Errorf("wait for snapshot restore guest agent readiness: %w", err)
			}
			return nil
		},
	}, snapshotstore.RestoreWorkflowRequest{
		SourceInstanceID: id,
		SnapshotID:       snapshotID,
		TargetState:      string(req.TargetState),
		TargetHypervisor: string(req.TargetHypervisor),
	})
	if err != nil {
		return nil, m.mapWorkflowError(err)
	}

	return m.getInstance(ctx, instanceID)
}

func (m *manager) forkSnapshot(ctx context.Context, snapshotID string, req ForkSnapshotRequest) (*Instance, error) {
	forkID, err := snapshotstore.RunForkWorkflow(ctx, snapshotstore.ForkWorkflowDeps{
		ValidateRequest: func(in snapshotstore.ForkWorkflowRequest) error {
			return snapshotstore.ValidateForkRequest(in.Name, in.TargetState, validateInstanceName)
		},
		LoadRecord: func(ctx context.Context, snapshotID string) (*snapshotstore.Record, error) {
			_ = ctx
			record, err := m.snapshotStore().LoadRecord(snapshotID)
			if err != nil {
				if errors.Is(err, snapshotstore.ErrNotFound) {
					return nil, ErrSnapshotNotFound
				}
				return nil, err
			}
			return record, nil
		},
		ValidateRecordVolumeSafety: func(ctx context.Context, record *snapshotstore.Record) error {
			_ = ctx
			stored, err := snapshotstore.DecodeRecordMetadata[StoredMetadata](record)
			if err != nil {
				return err
			}
			if err := validateForkVolumeSafety(stored.Volumes); err != nil {
				return fmt.Errorf("%w: snapshot requires readonly volume attachments: %v", ErrNotSupported, err)
			}
			return nil
		},
		RecordNetworkEnabled: func(ctx context.Context, record *snapshotstore.Record) (bool, error) {
			_ = ctx
			stored, err := snapshotstore.DecodeRecordMetadata[StoredMetadata](record)
			if err != nil {
				return false, err
			}
			return stored.NetworkEnabled, nil
		},
		EnsureInstanceNameAvailable: func(ctx context.Context, name string, networkEnabled bool) error {
			return m.ensureInstanceNameAvailableForSnapshotFork(ctx, name, networkEnabled)
		},
		ResolveTargetHypervisor: func(ctx context.Context, record *snapshotstore.Record, requested string) (string, error) {
			_ = ctx
			stored, err := snapshotstore.DecodeRecordMetadata[StoredMetadata](record)
			if err != nil {
				return "", err
			}
			hv, err := m.resolveSnapshotTargetHypervisor(record.Snapshot.Kind, stored.HypervisorType, hypervisor.Type(requested))
			if err != nil {
				return "", err
			}
			return string(hv), nil
		},
		GenerateInstanceID: func() string {
			return cuid2.Generate()
		},
		InstanceIDExists: func(ctx context.Context, instanceID string) (bool, error) {
			_ = ctx
			_, err := m.loadMetadata(instanceID)
			if err == nil {
				return true, nil
			}
			if errors.Is(err, ErrNotFound) {
				return false, nil
			}
			return false, err
		},
		PrepareFork: func(ctx context.Context, record *snapshotstore.Record, forkID string, in snapshotstore.ForkWorkflowRequest, targetHypervisor string) error {
			return m.prepareForkFromSnapshotRecord(ctx, in.SnapshotID, forkID, in.Name, record, hypervisor.Type(targetHypervisor))
		},
		ApplyForkTargetState: func(ctx context.Context, forkID, targetState string) (string, error) {
			inst, err := m.applyForkTargetState(ctx, forkID, State(targetState))
			if err != nil {
				return "", err
			}
			return string(inst.State), nil
		},
		EnsureGuestAgentReadyForFork: func(ctx context.Context, forkID string) error {
			inst, err := m.getInstance(ctx, forkID)
			if err != nil {
				return err
			}
			return ensureGuestAgentReadyForForkPhase(ctx, &inst.StoredMetadata, "before returning running fork instance")
		},
		CleanupForkOnError: func(ctx context.Context, forkID string) error {
			return m.cleanupForkInstanceOnError(ctx, forkID)
		},
	}, snapshotstore.ForkWorkflowRequest{
		SnapshotID:       snapshotID,
		Name:             req.Name,
		TargetState:      string(req.TargetState),
		TargetHypervisor: string(req.TargetHypervisor),
	})
	if err != nil {
		return nil, m.mapWorkflowError(err)
	}

	return m.getInstance(ctx, forkID)
}

func (m *manager) resolveSnapshotTargetHypervisor(kind SnapshotKind, sourceHypervisor, requested hypervisor.Type) (hypervisor.Type, error) {
	return snapshotstore.ResolveTargetHypervisor(kind, sourceHypervisor, requested, func(h hypervisor.Type) error {
		_, err := m.getVMStarter(h)
		return err
	})
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

func (m *manager) ensureGuestAgentReadyByInstanceID(ctx context.Context, instanceID, phase string) error {
	meta, err := m.loadMetadata(instanceID)
	if err != nil {
		return err
	}
	inst := m.toInstance(ctx, meta)
	return ensureGuestAgentReadyForForkPhase(ctx, &inst.StoredMetadata, phase)
}

func (m *manager) buildSnapshotStoreRecord(ctx context.Context, sourceInstanceID string, req CreateSnapshotRequest, snapshotID string) (*snapshotstore.Record, error) {
	meta, err := m.loadMetadata(sourceInstanceID)
	if err != nil {
		return nil, err
	}
	stored := cloneStoredMetadataForFork(meta.StoredMetadata)
	sizeBytes, err := snapshotstore.DirectoryFileSize(m.paths.SnapshotGuestDir(snapshotID))
	if err != nil {
		return nil, err
	}
	return snapshotstore.BuildRecord(Snapshot{
		Id:               snapshotID,
		Name:             req.Name,
		Kind:             req.Kind,
		SourceInstanceID: stored.Id,
		SourceName:       stored.Name,
		SourceHypervisor: stored.HypervisorType,
		CreatedAt:        time.Now(),
		SizeBytes:        sizeBytes,
	},
		stored)
}

func (m *manager) prepareRestoreMetadata(ctx context.Context, sourceInstanceID string, kind SnapshotKind, stored StoredMetadata, targetHypervisor hypervisor.Type) error {
	log := logger.FromContext(ctx)
	sourceMeta, err := m.loadMetadata(sourceInstanceID)
	if err != nil {
		return err
	}

	restored := cloneStoredMetadataForFork(stored)
	restored.Id = sourceMeta.Id
	restored.Name = sourceMeta.Name
	restored.DataDir = m.paths.InstanceDir(sourceInstanceID)
	restored.HypervisorPID = nil
	restored.StartedAt = nil
	restored.StoppedAt = nil
	restored.ExitCode = nil
	restored.ExitMessage = ""
	restored.HypervisorType = targetHypervisor

	starter, err := m.getVMStarter(targetHypervisor)
	if err != nil {
		return fmt.Errorf("get vm starter: %w", err)
	}
	hvVersion, err := starter.GetVersion(m.paths)
	if err != nil {
		log.WarnContext(ctx, "failed to get hypervisor version", "hypervisor", targetHypervisor, "error", err)
		hvVersion = "unknown"
	}
	restored.HypervisorVersion = hvVersion
	restored.SocketPath = m.paths.InstanceSocket(sourceInstanceID, starter.SocketName())
	restored.VsockSocket = m.paths.InstanceSocket(sourceInstanceID, hypervisor.VsockSocketNameForType(targetHypervisor))
	if kind == SnapshotKindStopped {
		restored.VsockCID = generateVsockCID(sourceInstanceID)
	}

	if err := m.saveMetadata(&metadata{StoredMetadata: restored}); err != nil {
		return fmt.Errorf("save restored metadata: %w", err)
	}
	return nil
}

func (m *manager) prepareForkFromSnapshotRecord(ctx context.Context, snapshotID, forkID, forkName string, record *snapshotstore.Record, targetHypervisor hypervisor.Type) error {
	stored, err := snapshotstore.DecodeRecordMetadata[StoredMetadata](record)
	if err != nil {
		return err
	}

	dstDir := m.paths.InstanceDir(forkID)
	cu := cleanup.Make(func() {
		_ = os.RemoveAll(dstDir)
	})
	defer cu.Clean()

	if err := snapshotstore.CopyPayload(m.paths.SnapshotGuestDir(snapshotID), dstDir, "clone snapshot payload"); err != nil {
		return err
	}

	starter, err := m.getVMStarter(targetHypervisor)
	if err != nil {
		return fmt.Errorf("get vm starter: %w", err)
	}
	hvVersion, err := starter.GetVersion(m.paths)
	if err != nil {
		hvVersion = "unknown"
	}

	now := time.Now()
	forkMeta := cloneStoredMetadataForFork(stored)
	forkMeta.Id = forkID
	forkMeta.Name = forkName
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
	if record.Snapshot.Kind == SnapshotKindStandby {
		forkMeta.VsockCID = stored.VsockCID
	} else {
		forkMeta.VsockCID = generateVsockCID(forkID)
	}
	if forkMeta.NetworkEnabled {
		forkMeta.IP = ""
		forkMeta.MAC = ""
	}

	if record.Snapshot.Kind == SnapshotKindStandby {
		netCfg := (*hypervisor.ForkNetworkConfig)(nil)
		if forkMeta.NetworkEnabled {
			netCfg = &hypervisor.ForkNetworkConfig{TAPDevice: network.GenerateTAPName(forkID)}
		}
		if _, err := starter.PrepareFork(ctx, hypervisor.ForkPrepareRequest{
			SnapshotConfigPath: m.paths.InstanceSnapshotConfig(forkID),
			SourceDataDir:      stored.DataDir,
			TargetDataDir:      forkMeta.DataDir,
			VsockCID:           forkMeta.VsockCID,
			VsockSocket:        forkMeta.VsockSocket,
			SerialLogPath:      m.paths.InstanceAppLog(forkID),
			Network:            netCfg,
		}); err != nil {
			if errors.Is(err, hypervisor.ErrNotSupported) {
				return fmt.Errorf("%w: snapshot fork is not supported for hypervisor %s", ErrNotSupported, targetHypervisor)
			}
			return fmt.Errorf("prepare snapshot fork state: %w", err)
		}
	}

	if err := m.saveMetadata(&metadata{StoredMetadata: forkMeta}); err != nil {
		return fmt.Errorf("save fork metadata: %w", err)
	}

	cu.Release()
	return nil
}

func (m *manager) mapWorkflowError(err error) error {
	switch {
	case errors.Is(err, snapshotstore.ErrInvalidRequest):
		return fmt.Errorf("%w: %v", ErrInvalidRequest, err)
	case errors.Is(err, snapshotstore.ErrInvalidState):
		return fmt.Errorf("%w: %v", ErrInvalidState, err)
	case errors.Is(err, snapshotstore.ErrAlreadyExists):
		return fmt.Errorf("%w: %v", ErrAlreadyExists, err)
	default:
		return err
	}
}
