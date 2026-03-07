package snapshot

import (
	"context"
	"errors"
	"fmt"
	"os"
)

var (
	ErrInvalidRequest = errors.New("invalid request")
	ErrInvalidState   = errors.New("invalid state")
	ErrAlreadyExists  = errors.New("already exists")
)

type SourceInfo struct {
	State string
}

type CreateWorkflowRequest struct {
	SourceInstanceID string
	Kind             SnapshotKind
	Name             string
}

type CreateWorkflowDeps struct {
	ValidateRequest             func(CreateWorkflowRequest) error
	LoadSource                  func(context.Context, string) (*SourceInfo, error)
	EnsureSnapshotNameAvailable func(context.Context, string, string) error
	GenerateSnapshotID          func() string
	SnapshotIDExists            func(context.Context, string) (bool, error)
	EnsureGuestAgentReady       func(context.Context, string, string) error
	StandbySource               func(context.Context, string) error
	RestoreSource               func(context.Context, string) error
	CopySourceToSnapshot        func(context.Context, string, string) error
	BuildRecord                 func(context.Context, CreateWorkflowRequest, string) (*Record, error)
	SaveRecord                  func(context.Context, *Record) error
	EnsureSnapshotStoreDir      func(context.Context) error
	RemoveSnapshotDir           func(context.Context, string) error
}

func RunCreateWorkflow(ctx context.Context, deps CreateWorkflowDeps, req CreateWorkflowRequest) (*Snapshot, error) {
	if err := deps.ValidateRequest(req); err != nil {
		return nil, err
	}
	source, err := deps.LoadSource(ctx, req.SourceInstanceID)
	if err != nil {
		return nil, err
	}
	if err := deps.EnsureSnapshotNameAvailable(ctx, req.SourceInstanceID, req.Name); err != nil {
		return nil, err
	}

	snapshotID := deps.GenerateSnapshotID()
	exists, err := deps.SnapshotIDExists(ctx, snapshotID)
	if err != nil {
		return nil, err
	}
	if exists {
		return nil, fmt.Errorf("%w: generated snapshot id already exists", ErrAlreadyExists)
	}

	if err := deps.EnsureSnapshotStoreDir(ctx); err != nil {
		return nil, err
	}

	cleanupSnapshotDir := true
	defer func() {
		if cleanupSnapshotDir {
			_ = deps.RemoveSnapshotDir(context.Background(), snapshotID)
		}
	}()

	switch req.Kind {
	case SnapshotKindStandby:
		restoreSource := false
		switch source.State {
		case StateRunning:
			if err := deps.EnsureGuestAgentReady(ctx, req.SourceInstanceID, "before running snapshot"); err != nil {
				return nil, err
			}
			if err := deps.StandbySource(ctx, req.SourceInstanceID); err != nil {
				return nil, fmt.Errorf("standby source instance: %w", err)
			}
			restoreSource = true
		case StateStandby:
			// already in standby
		default:
			return nil, fmt.Errorf("%w: standby snapshot requires source in %s or %s, got %s", ErrInvalidState, StateRunning, StateStandby, source.State)
		}

		copyErr := deps.CopySourceToSnapshot(ctx, req.SourceInstanceID, snapshotID)
		var record *Record
		if copyErr == nil {
			record, copyErr = deps.BuildRecord(ctx, req, snapshotID)
		}

		if restoreSource {
			restoreErr := deps.RestoreSource(ctx, req.SourceInstanceID)
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
		if err := deps.SaveRecord(ctx, record); err != nil {
			return nil, err
		}
		cleanupSnapshotDir = false
		out := record.Snapshot
		return &out, nil

	case SnapshotKindStopped:
		if source.State != StateStopped {
			return nil, fmt.Errorf("%w: stopped snapshot requires source in %s, got %s", ErrInvalidState, StateStopped, source.State)
		}
		if err := deps.CopySourceToSnapshot(ctx, req.SourceInstanceID, snapshotID); err != nil {
			return nil, err
		}
		record, err := deps.BuildRecord(ctx, req, snapshotID)
		if err != nil {
			return nil, err
		}
		if err := deps.SaveRecord(ctx, record); err != nil {
			return nil, err
		}
		cleanupSnapshotDir = false
		out := record.Snapshot
		return &out, nil
	default:
		return nil, fmt.Errorf("%w: unsupported snapshot kind %q", ErrInvalidRequest, req.Kind)
	}
}

type RestoreWorkflowRequest struct {
	SourceInstanceID string
	SnapshotID       string
	TargetState      string
	TargetHypervisor string
}

type RestoreWorkflowDeps struct {
	LoadRecord                func(context.Context, string) (*Record, error)
	LoadSource                func(context.Context, string) (*SourceInfo, error)
	ResolveTargetHypervisor   func(context.Context, *Record, string) (string, error)
	ReplaceInstanceFromRecord func(context.Context, string, string) error
	PrepareRestoreMetadata    func(context.Context, string, *Record, string) error
	RemoveInstanceSnapshot    func(context.Context, string) error
	RestoreToRunning          func(context.Context, string) error
	StartToRunning            func(context.Context, string) error
}

func RunRestoreWorkflow(ctx context.Context, deps RestoreWorkflowDeps, req RestoreWorkflowRequest) (string, error) {
	record, err := deps.LoadRecord(ctx, req.SnapshotID)
	if err != nil {
		return "", err
	}
	if record.Snapshot.SourceInstanceID != req.SourceInstanceID {
		return "", fmt.Errorf("%w: snapshot %s belongs to instance %s", ErrInvalidRequest, req.SnapshotID, record.Snapshot.SourceInstanceID)
	}

	source, err := deps.LoadSource(ctx, req.SourceInstanceID)
	if err != nil {
		return "", err
	}
	if source.State == StateRunning {
		return "", fmt.Errorf("%w: cannot restore snapshot while source is %s", ErrInvalidState, source.State)
	}

	targetState, err := ResolveTargetState(record.Snapshot.Kind, req.TargetState)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrInvalidRequest, err)
	}
	targetHypervisor, err := deps.ResolveTargetHypervisor(ctx, record, req.TargetHypervisor)
	if err != nil {
		return "", err
	}

	if err := deps.ReplaceInstanceFromRecord(ctx, req.SnapshotID, req.SourceInstanceID); err != nil {
		return "", err
	}
	if err := deps.PrepareRestoreMetadata(ctx, req.SourceInstanceID, record, targetHypervisor); err != nil {
		return "", err
	}

	switch record.Snapshot.Kind {
	case SnapshotKindStandby:
		switch targetState {
		case StateStandby:
			return req.SourceInstanceID, nil
		case StateStopped:
			if err := deps.RemoveInstanceSnapshot(ctx, req.SourceInstanceID); err != nil {
				return "", err
			}
			return req.SourceInstanceID, nil
		case StateRunning:
			if err := deps.RestoreToRunning(ctx, req.SourceInstanceID); err != nil {
				return "", err
			}
			return req.SourceInstanceID, nil
		}
	case SnapshotKindStopped:
		switch targetState {
		case StateStopped:
			_ = deps.RemoveInstanceSnapshot(ctx, req.SourceInstanceID)
			return req.SourceInstanceID, nil
		case StateRunning:
			if err := deps.StartToRunning(ctx, req.SourceInstanceID); err != nil {
				return "", err
			}
			return req.SourceInstanceID, nil
		}
	}

	return "", fmt.Errorf("unsupported restore target state %s for snapshot kind %s", targetState, record.Snapshot.Kind)
}

type ForkWorkflowRequest struct {
	SnapshotID       string
	Name             string
	TargetState      string
	TargetHypervisor string
}

type ForkWorkflowDeps struct {
	ValidateRequest              func(ForkWorkflowRequest) error
	LoadRecord                   func(context.Context, string) (*Record, error)
	ValidateRecordVolumeSafety   func(context.Context, *Record) error
	RecordNetworkEnabled         func(context.Context, *Record) (bool, error)
	EnsureInstanceNameAvailable  func(context.Context, string, bool) error
	ResolveTargetHypervisor      func(context.Context, *Record, string) (string, error)
	GenerateInstanceID           func() string
	InstanceIDExists             func(context.Context, string) (bool, error)
	PrepareFork                  func(context.Context, *Record, string, ForkWorkflowRequest, string) error
	ApplyForkTargetState         func(context.Context, string, string) (string, error)
	EnsureGuestAgentReadyForFork func(context.Context, string) error
	CleanupForkOnError           func(context.Context, string) error
}

func RunForkWorkflow(ctx context.Context, deps ForkWorkflowDeps, req ForkWorkflowRequest) (string, error) {
	if err := deps.ValidateRequest(req); err != nil {
		return "", err
	}
	record, err := deps.LoadRecord(ctx, req.SnapshotID)
	if err != nil {
		return "", err
	}
	if err := deps.ValidateRecordVolumeSafety(ctx, record); err != nil {
		return "", err
	}
	networkEnabled, err := deps.RecordNetworkEnabled(ctx, record)
	if err != nil {
		return "", err
	}
	if err := deps.EnsureInstanceNameAvailable(ctx, req.Name, networkEnabled); err != nil {
		return "", err
	}

	targetState, err := ResolveTargetState(record.Snapshot.Kind, req.TargetState)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrInvalidRequest, err)
	}
	targetHypervisor, err := deps.ResolveTargetHypervisor(ctx, record, req.TargetHypervisor)
	if err != nil {
		return "", err
	}

	forkID := deps.GenerateInstanceID()
	exists, err := deps.InstanceIDExists(ctx, forkID)
	if err != nil {
		return "", err
	}
	if exists {
		return "", fmt.Errorf("%w: generated fork id already exists", ErrAlreadyExists)
	}

	if err := deps.PrepareFork(ctx, record, forkID, req, targetHypervisor); err != nil {
		return "", err
	}

	finalState, err := deps.ApplyForkTargetState(ctx, forkID, targetState)
	if err != nil {
		if cleanupErr := deps.CleanupForkOnError(ctx, forkID); cleanupErr != nil {
			return "", fmt.Errorf("apply snapshot fork target state: %w; additionally failed to cleanup forked instance %s: %v", err, forkID, cleanupErr)
		}
		return "", fmt.Errorf("apply snapshot fork target state: %w", err)
	}

	if finalState == StateRunning {
		if err := deps.EnsureGuestAgentReadyForFork(ctx, forkID); err != nil {
			if cleanupErr := deps.CleanupForkOnError(ctx, forkID); cleanupErr != nil {
				return "", fmt.Errorf("wait for fork guest agent readiness: %w; additionally failed to cleanup forked instance %s: %v", err, forkID, cleanupErr)
			}
			return "", fmt.Errorf("wait for fork guest agent readiness: %w", err)
		}
	}

	return forkID, nil
}

func EnsureDir(path string) error {
	return os.MkdirAll(path, 0755)
}
