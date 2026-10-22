package instances

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/kernel/hypeman/lib/forkvm"
	"github.com/kernel/hypeman/lib/hypervisor"
	"github.com/kernel/hypeman/lib/hypervisor/vz/shimconfig"
	"github.com/kernel/hypeman/lib/instances/phasetracking"
	"github.com/kernel/hypeman/lib/paths"
	restartpolicy "github.com/kernel/hypeman/lib/restart-policy"
	"github.com/kernel/hypeman/lib/snapshot"
)

func validateMacOSSnapshotCreate(meta *metadata, req CreateSnapshotRequest) error {
	if meta.MacOS == nil {
		return nil
	}
	if req.Kind == SnapshotKindStandby {
		if err := validateMacOSMemoryCompression(req.Compression); err != nil {
			return err
		}
		return validateMacOSMemorySnapshotSupport(&meta.StoredMetadata)
	}
	if req.Kind != SnapshotKindStopped {
		return fmt.Errorf("%w: unsupported macOS snapshot kind %q", ErrInvalidRequest, req.Kind)
	}
	if !meta.StopGraceful {
		return fmt.Errorf("%w: macOS stopped snapshots require a graceful stop", ErrInvalidState)
	}
	return nil
}

// validateMacOSMemoryCompression rejects an explicit compression request. macOS saved
// state is never compressed, so accepting one would silently drop it.
func validateMacOSMemoryCompression(compression *snapshot.SnapshotCompressionConfig) error {
	if compression != nil && compression.Enabled {
		return fmt.Errorf("%w: macOS memory snapshots do not support compression", ErrInvalidRequest)
	}
	return nil
}

func validateMacOSMemorySnapshotSupport(stored *StoredMetadata) error {
	if stored.MacOS == nil {
		return nil
	}
	caps, ok := hypervisor.CapabilitiesForType(stored.HypervisorType)
	if !ok || !caps.SupportsMacOSBoot || !caps.SupportsSnapshot {
		return fmt.Errorf("%w: macOS memory snapshots require a host with VZ save/restore support", ErrNotSupported)
	}
	return nil
}

func validateMacOSSaveRestoreSupport(ctx context.Context, stored *StoredMetadata, hv hypervisor.Hypervisor) error {
	if stored.MacOS == nil {
		return nil
	}
	validator, ok := hv.(hypervisor.SnapshotSupportValidator)
	if !ok {
		return fmt.Errorf("%w: macOS snapshot configuration validation is unavailable", ErrNotSupported)
	}
	if err := validator.ValidateSnapshotSupport(ctx); err != nil {
		if errors.Is(err, hypervisor.ErrNotSupported) {
			return fmt.Errorf("%w: %w", ErrNotSupported, err)
		}
		return fmt.Errorf("validate macOS snapshot configuration: %w", err)
	}
	return nil
}

func (m *manager) restoreSnapshotSource(ctx context.Context, stored *StoredMetadata) error {
	if stored.MacOS != nil {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
		defer cancel()
	}
	_, err := m.restoreInstance(ctx, stored.Id)
	return err
}

func resumeSnapshotAfterError(ctx context.Context, stored *StoredMetadata, hv hypervisor.Hypervisor) error {
	if stored.MacOS != nil {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
	}
	return hv.Resume(ctx)
}

func (m *manager) validateMacOSSnapshotSchedule(ctx context.Context, meta *metadata) error {
	if meta.MacOS == nil {
		return nil
	}
	if err := validateMacOSMemorySnapshotSupport(&meta.StoredMetadata); err != nil {
		return err
	}
	inst := m.toInstance(ctx, meta)
	switch inst.State {
	case StateRunning, StateInitializing, StateCreated, StatePaused:
		hv, err := m.getHypervisor(inst.SocketPath, inst.HypervisorType)
		if err != nil {
			return err
		}
		return validateMacOSSaveRestoreSupport(ctx, &meta.StoredMetadata, hv)
	}
	return nil
}

func prepareMacOSSnapshotRestoreMetadata(restored *StoredMetadata, source *StoredMetadata, kind SnapshotKind, target State) {
	restored.Phases = source.Phases
	phase := phasetracking.PhaseStopped
	if kind == SnapshotKindStandby {
		restored.StopGraceful = false
		if target != StateStopped {
			phase = phasetracking.PhaseStandby
		}
	}
	restored.Phases.Record(phase, time.Now().UTC())
	// A restored VM is a new run, so it must not inherit the source's restart block.
	restored.RestartStatus = restartpolicy.Status{}
	if target == StateStopped {
		restored.RestartStatus = restartpolicy.Status{BlockedReason: restartpolicy.BlockedReasonManualStop}
	}
}

func validateMacOSSnapshotRestoreState(inst *Instance) error {
	if inst.MacOS != nil && inst.State != StateStopped && inst.State != StateStandby {
		return fmt.Errorf("%w: macOS snapshot restore requires a stopped or standby instance, got %s", ErrInvalidState, inst.State)
	}
	return nil
}

// The manifest is used by VZ rather than rebuilt from instance metadata. Validate
// the saved configuration and its disk/aux/memory bundle before replacing storage.
func (m *manager) validateMacOSMemorySnapshotPayload(guestDir string, stored *StoredMetadata) error {
	if stored.MacOS == nil {
		return nil
	}
	snapshotDir := filepath.Join(guestDir, "snapshots", "snapshot-latest")
	data, err := os.ReadFile(filepath.Join(snapshotDir, shimconfig.SnapshotManifestFile))
	if err != nil {
		return fmt.Errorf("%w: read macOS snapshot manifest: %v", ErrInvalidRequest, err)
	}
	var manifest shimconfig.SnapshotManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return fmt.Errorf("%w: decode macOS snapshot manifest: %v", ErrInvalidRequest, err)
	}
	cfg := manifest.ShimConfig
	origin := m.paths.InstanceDir(stored.Id)
	if manifest.Hypervisor != string(hypervisor.TypeVZ) || (manifest.MachineStateFile != "" && manifest.MachineStateFile != shimconfig.SnapshotMachineStateFile) {
		return fmt.Errorf("%w: invalid macOS snapshot format", ErrInvalidRequest)
	}
	if cfg.VCPUs != stored.Vcpus || cfg.MemoryBytes != stored.Size || cfg.MacHardwareModelData != base64.StdEncoding.EncodeToString(stored.MacOS.HardwareModel) || cfg.MacMachineIdentifierData != base64.StdEncoding.EncodeToString(stored.MacOS.MachineIdentifier) {
		return fmt.Errorf("%w: macOS snapshot configuration does not match its metadata", ErrInvalidRequest)
	}
	if len(cfg.Disks) != 1 || cfg.Disks[0].Readonly || cfg.Disks[0].Path != m.paths.InstanceMacOSBootDisk(stored.Id) || cfg.MacAuxStoragePath != filepath.Join(origin, macOSAuxStorageFile) {
		return fmt.Errorf("%w: macOS snapshot storage configuration does not match its instance", ErrInvalidRequest)
	}
	if cfg.KernelPath != "" || cfg.InitrdPath != "" || cfg.EnableMemoryBalloon || cfg.RequireMemoryBalloon || cfg.EnableRosetta {
		return fmt.Errorf("%w: unsupported macOS snapshot devices", ErrInvalidRequest)
	}
	if stored.NetworkEnabled {
		if len(cfg.Networks) != 1 || !strings.EqualFold(cfg.Networks[0].MAC, stored.MAC) || !strings.EqualFold(stored.MAC, stored.MacOS.MAC) {
			return fmt.Errorf("%w: macOS snapshot network identity does not match its metadata", ErrInvalidRequest)
		}
	} else if len(cfg.Networks) != 0 {
		return fmt.Errorf("%w: macOS snapshot unexpectedly enables networking", ErrInvalidRequest)
	}
	for _, path := range []string{filepath.Join(guestDir, paths.MacOSBootDiskFile), filepath.Join(guestDir, macOSAuxStorageFile), filepath.Join(snapshotDir, shimconfig.SnapshotMachineStateFile)} {
		info, err := os.Stat(path)
		if err != nil || !info.Mode().IsRegular() || info.Size() == 0 {
			return fmt.Errorf("%w: missing or invalid macOS snapshot payload %s", ErrInvalidRequest, filepath.Base(path))
		}
	}
	return nil
}

func validateMacOSSnapshotRestore(source *metadata, rec *snapshotRecord) error {
	if source.MacOS == nil && rec.StoredMetadata.MacOS == nil {
		return nil
	}
	if source.MacOS == nil || rec.StoredMetadata.MacOS == nil {
		return fmt.Errorf("%w: snapshot and instance guest types differ", ErrInvalidRequest)
	}
	if rec.Snapshot.Kind != SnapshotKindStopped && rec.Snapshot.Kind != SnapshotKindStandby {
		return fmt.Errorf("%w: unsupported macOS snapshot kind %q", ErrInvalidRequest, rec.Snapshot.Kind)
	}
	if rec.Snapshot.Kind == SnapshotKindStandby {
		if err := validateMacOSMemorySnapshotSupport(&rec.StoredMetadata); err != nil {
			return err
		}
	}
	want, got := source.MacOS, rec.StoredMetadata.MacOS
	if !bytes.Equal(want.MachineIdentifier, got.MachineIdentifier) || !bytes.Equal(want.HardwareModel, got.HardwareModel) || want.MAC != got.MAC {
		return fmt.Errorf("%w: snapshot identity does not match this instance", ErrInvalidRequest)
	}
	return nil
}

func validateMacOSSnapshotFork(rec *snapshotRecord) error {
	if rec.StoredMetadata.MacOS == nil {
		return nil
	}
	if rec.Snapshot.Kind != SnapshotKindStopped {
		return fmt.Errorf("%w: macOS forks require stopped snapshots", ErrNotSupported)
	}
	if !rec.StoredMetadata.StopGraceful {
		return fmt.Errorf("%w: macOS snapshot forks require a graceful stop receipt", ErrInvalidState)
	}
	if !rec.StoredMetadata.GuestAgentEnabled() {
		return fmt.Errorf("%w: macOS snapshot forks require an enabled guest agent for identity rekey", ErrInvalidRequest)
	}
	return nil
}

func (m *manager) saveMacOSSnapshotFork(ctx context.Context, meta *metadata) error {
	if err := prepareMacOSClone(&meta.StoredMetadata); err != nil {
		return err
	}
	meta.ProgramStartedAt = nil
	meta.GuestAgentReadyAt = nil
	meta.GuestAgent = nil
	meta.Phases.Reset()
	meta.Phases.Record(phasetracking.PhaseStopped, meta.CreatedAt)
	return m.saveForkMetadata(ctx, meta)
}

// replaceMacOSInstanceWithSnapshotPayload stages the snapshot and restored metadata
// beside the instance, then swaps both in. A failure before the swap leaves the
// previous directory intact; a failed swap is rolled back. The staging root lives outside the guests directory,
// so instance and snapshot listings never see a half-restored copy.
func (m *manager) replaceMacOSInstanceWithSnapshotPayload(snapshotID string, restored *metadata) error {
	instanceID := restored.Id
	if err := m.recoverMacOSRestoreStaging(instanceID); err != nil {
		return fmt.Errorf("recover before restore: %w", err)
	}
	instanceDir := m.paths.InstanceDir(instanceID)
	root := filepath.Join(m.paths.DataDir(), "restore-staging", instanceID)
	staged := filepath.Join(root, "staged")
	previous := filepath.Join(root, "previous")
	fail := func(err error) error {
		_ = os.RemoveAll(root)
		return err
	}
	if err := os.RemoveAll(root); err != nil {
		return fmt.Errorf("clear restore staging: %w", err)
	}
	if err := os.MkdirAll(root, 0700); err != nil {
		return fmt.Errorf("create restore staging: %w", err)
	}
	if err := forkvm.CopyGuestDirectory(m.paths.SnapshotGuestDir(snapshotID), staged); err != nil {
		if errors.Is(err, forkvm.ErrSparseCopyUnsupported) {
			return fail(fmt.Errorf("restore requires sparse-capable filesystem (SEEK_DATA/SEEK_HOLE unsupported): %w", err))
		}
		return fail(fmt.Errorf("stage snapshot payload: %w", err))
	}
	if restored.Phases.Current == phasetracking.PhaseStopped {
		if err := os.RemoveAll(filepath.Join(staged, "snapshots", "snapshot-latest")); err != nil {
			return fail(fmt.Errorf("discard staged memory checkpoint: %w", err))
		}
	}
	data, err := json.MarshalIndent(restored, "", "  ")
	if err != nil {
		return fail(fmt.Errorf("marshal restored metadata: %w", err))
	}
	if err := os.WriteFile(filepath.Join(staged, filepath.Base(m.paths.InstanceMetadata(instanceID))), data, 0644); err != nil {
		return fail(fmt.Errorf("stage restored metadata: %w", err))
	}
	if err := os.Rename(instanceDir, previous); err != nil {
		return fail(fmt.Errorf("retire instance directory: %w", err))
	}
	if err := os.Rename(staged, instanceDir); err != nil {
		if rollbackErr := os.Rename(previous, instanceDir); rollbackErr != nil {
			return fmt.Errorf("install snapshot payload: %w (rollback failed: %v; previous state kept at %s)", err, rollbackErr, previous)
		}
		return fail(fmt.Errorf("install snapshot payload: %w", err))
	}
	_ = os.RemoveAll(root)
	return nil
}

// recoverMacOSRestoreStaging resolves a swap interrupted by a crash. With the instance
// directory missing and the previous directory parked, the previous state is put back.
// If the instance directory is present, a completed swap leaves stale staging, which is
// removed. Staging with no previous directory is an unfinished copy and is removed.
// An empty instanceID recovers every instance.
func (m *manager) recoverMacOSRestoreStaging(instanceID string) error {
	root := filepath.Join(m.paths.DataDir(), "restore-staging")
	ids := []string{instanceID}
	if instanceID == "" {
		entries, err := os.ReadDir(root)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		ids = ids[:0]
		for _, entry := range entries {
			ids = append(ids, entry.Name())
		}
	}
	for _, id := range ids {
		stagingRoot := filepath.Join(root, id)
		previous := filepath.Join(stagingRoot, "previous")
		instanceDir := m.paths.InstanceDir(id)
		_, previousErr := os.Stat(previous)
		if previousErr != nil && !errors.Is(previousErr, os.ErrNotExist) {
			return fmt.Errorf("inspect restore staging for %s: %w", id, previousErr)
		}
		if previousErr == nil {
			_, instanceErr := os.Stat(instanceDir)
			if instanceErr != nil && !errors.Is(instanceErr, os.ErrNotExist) {
				return fmt.Errorf("inspect instance directory for %s: %w", id, instanceErr)
			}
			if errors.Is(instanceErr, os.ErrNotExist) {
				if err := os.MkdirAll(filepath.Dir(instanceDir), 0755); err != nil {
					return fmt.Errorf("prepare rollback for %s: %w", id, err)
				}
				if err := os.Rename(previous, instanceDir); err != nil {
					return fmt.Errorf("roll back restore for %s: %w", id, err)
				}
			}
		}
		if err := os.RemoveAll(stagingRoot); err != nil {
			return fmt.Errorf("clear restore staging for %s: %w", id, err)
		}
	}
	return nil
}

// finishMacOSResume completes a restore whose API process died after the shim had
// loaded the VM, once resuming the Paused guest succeeds. It records the shim, and
// removes the consumed memory snapshot so a later crash cannot restore stale memory
// onto the guest's newer disk.
func (m *manager) finishMacOSResume(id string) error {
	meta, err := m.loadMetadata(id)
	if err != nil {
		return err
	}
	if meta.HypervisorPID == nil || !ProcessExists(*meta.HypervisorPID) {
		pid, err := vzShimOwner(meta.SocketPath)
		if err != nil {
			return err
		}
		if pid > 0 {
			meta.HypervisorProcessIdentity.Set(pid)
		}
	}
	if err := os.RemoveAll(m.paths.InstanceSnapshotLatest(id)); err != nil {
		return fmt.Errorf("remove consumed snapshot: %w", err)
	}
	meta.Phases.Record(initialBootPhase(&meta.StoredMetadata), time.Now().UTC())
	return m.saveMetadata(meta)
}
