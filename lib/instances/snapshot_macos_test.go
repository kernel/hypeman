package instances

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/kernel/hypeman/lib/hypervisor"
	"github.com/kernel/hypeman/lib/images"
	"github.com/kernel/hypeman/lib/instances/phasetracking"
	"github.com/kernel/hypeman/lib/paths"
	restartpolicy "github.com/kernel/hypeman/lib/restart-policy"
	"github.com/kernel/hypeman/lib/snapshot"
	"github.com/stretchr/testify/require"
)

func macOSGuestMetadata(identity byte) *StoredMetadata {
	return &StoredMetadata{MacOS: &images.MacOSImage{
		HardwareModel: []byte{1}, MachineIdentifier: []byte{identity}, MAC: "02:00:00:00:00:01", CPUs: 4, Memory: 8 << 30,
	}}
}

func TestMacOSSnapshotCreateGuardsGracefulStopAndHostSupport(t *testing.T) {
	require.NoError(t, validateMacOSSnapshotCreate(&metadata{StoredMetadata: StoredMetadata{}}, CreateSnapshotRequest{Kind: SnapshotKindStandby}), "Linux guests keep their rules")

	mac := &metadata{StoredMetadata: *macOSGuestMetadata(2)}
	require.ErrorIs(t, validateMacOSSnapshotCreate(mac, CreateSnapshotRequest{Kind: SnapshotKindStandby}), ErrNotSupported)
	require.ErrorIs(t, validateMacOSSnapshotCreate(mac, CreateSnapshotRequest{Kind: SnapshotKindStopped}), ErrInvalidState, "a forced stop is not a consistent disk")

	mac.StopGraceful = true
	require.NoError(t, validateMacOSSnapshotCreate(mac, CreateSnapshotRequest{Kind: SnapshotKindStopped}))
}

func TestMacOSSnapshotRestoreRequiresSameIdentityAndSupportedKind(t *testing.T) {
	source := &metadata{StoredMetadata: *macOSGuestMetadata(2)}
	same := &snapshotRecord{Snapshot: Snapshot{Kind: SnapshotKindStopped}, StoredMetadata: *macOSGuestMetadata(2)}
	require.NoError(t, validateMacOSSnapshotRestore(source, same))

	other := &snapshotRecord{Snapshot: Snapshot{Kind: SnapshotKindStopped}, StoredMetadata: *macOSGuestMetadata(9)}
	require.ErrorIs(t, validateMacOSSnapshotRestore(source, other), ErrInvalidRequest, "a different machine identifier is another guest")

	standby := &snapshotRecord{Snapshot: Snapshot{Kind: SnapshotKindStandby}, StoredMetadata: *macOSGuestMetadata(2)}
	require.ErrorIs(t, validateMacOSSnapshotRestore(source, standby), ErrNotSupported, "unsupported hosts cannot restore memory snapshots")

	linux := &snapshotRecord{Snapshot: Snapshot{Kind: SnapshotKindStopped}}
	require.ErrorIs(t, validateMacOSSnapshotRestore(source, linux), ErrInvalidRequest, "guest types must match")
}

func TestMacOSSnapshotForkRequiresGracefulStoppedSnapshotAndAgent(t *testing.T) {
	rec := &snapshotRecord{Snapshot: Snapshot{Kind: SnapshotKindStopped}, StoredMetadata: *macOSGuestMetadata(2)}
	require.ErrorIs(t, validateMacOSSnapshotFork(rec), ErrInvalidState)
	rec.StoredMetadata.StopGraceful = true
	require.ErrorIs(t, validateMacOSSnapshotFork(rec), ErrInvalidRequest)
	rec.StoredMetadata.MacOS.GuestAgent = true
	require.NoError(t, validateMacOSSnapshotFork(rec))
	rec.StoredMetadata.SkipGuestAgent = true
	require.ErrorIs(t, validateMacOSSnapshotFork(rec), ErrInvalidRequest)
	rec.StoredMetadata.SkipGuestAgent = false
	rec.Snapshot.Kind = SnapshotKindStandby
	require.ErrorIs(t, validateMacOSSnapshotFork(rec), ErrNotSupported)
	require.NoError(t, validateMacOSSnapshotFork(&snapshotRecord{}), "Linux admission is unchanged")
}

func writeGuestFiles(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(dir, 0700))
	for name, body := range files {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(body), 0600))
	}
}

func TestMacOSRestoreSwapsPayloadWithMetadata(t *testing.T) {
	m := &manager{paths: paths.New(t.TempDir())}
	const id, snap = "mac-restore", "snap-1"
	writeGuestFiles(t, m.paths.InstanceDir(id), map[string]string{"boot.raw": "after", macOSAuxStorageFile: "aux-after"})
	writeGuestFiles(t, m.paths.SnapshotGuestDir(snap), map[string]string{"boot.raw": "before", macOSAuxStorageFile: "aux-before", "metadata.json": "stale-metadata"})
	expiresAt := time.Now().Add(time.Hour)
	restored := &metadata{StoredMetadata: StoredMetadata{Id: id, Name: "current-name", ExpiresAt: &expiresAt, Vcpus: 4, Size: 8 << 30}}

	require.NoError(t, m.replaceMacOSInstanceWithSnapshotPayload(snap, restored))
	data, err := os.ReadFile(filepath.Join(m.paths.InstanceDir(id), "boot.raw"))
	require.NoError(t, err)
	require.Equal(t, "before", string(data))
	_, err = os.Stat(filepath.Join(m.paths.DataDir(), "restore-staging", id))
	require.ErrorIs(t, err, os.ErrNotExist, "staging is removed after a successful swap")
	// A restart before the caller's own saveMetadata must find the restored metadata.
	require.NoError(t, m.recoverMacOSRestoreStaging(id))
	got, err := m.loadMetadata(id)
	require.NoError(t, err)
	require.Equal(t, restored.Name, got.Name)
	require.Equal(t, restored.Vcpus, got.Vcpus)
	require.NotNil(t, got.ExpiresAt)
	require.True(t, expiresAt.Equal(*got.ExpiresAt))

	// A missing snapshot payload fails before the instance directory is retired.
	writeGuestFiles(t, m.paths.InstanceDir(id), map[string]string{"boot.raw": "current"})
	require.Error(t, m.replaceMacOSInstanceWithSnapshotPayload("missing-snapshot", &metadata{StoredMetadata: StoredMetadata{Id: id}}))
	data, err = os.ReadFile(filepath.Join(m.paths.InstanceDir(id), "boot.raw"))
	require.NoError(t, err)
	require.Equal(t, "current", string(data), "a failed staging leaves the previous disk untouched")
}

func TestMacOSRestoreRecoveryRollsBackAndCleans(t *testing.T) {
	const id = "mac-crash"
	newManager := func(t *testing.T) (*manager, string) {
		m := &manager{paths: paths.New(t.TempDir())}
		return m, filepath.Join(m.paths.DataDir(), "restore-staging", id)
	}

	// Crash after retiring the instance directory, before the swap: roll the previous state back.
	m, root := newManager(t)
	writeGuestFiles(t, filepath.Join(root, "previous"), map[string]string{"boot.raw": "previous"})
	require.NoError(t, m.recoverMacOSRestoreStaging(id))
	data, err := os.ReadFile(filepath.Join(m.paths.InstanceDir(id), "boot.raw"))
	require.NoError(t, err)
	require.Equal(t, "previous", string(data))
	_, err = os.Stat(root)
	require.ErrorIs(t, err, os.ErrNotExist)

	// Crash after a completed swap: the instance directory is current, so stale staging goes.
	m, root = newManager(t)
	writeGuestFiles(t, m.paths.InstanceDir(id), map[string]string{"boot.raw": "restored"})
	writeGuestFiles(t, filepath.Join(root, "previous"), map[string]string{"boot.raw": "old"})
	require.NoError(t, m.recoverMacOSRestoreStaging(id))
	data, err = os.ReadFile(filepath.Join(m.paths.InstanceDir(id), "boot.raw"))
	require.NoError(t, err)
	require.Equal(t, "restored", string(data))
	_, err = os.Stat(root)
	require.ErrorIs(t, err, os.ErrNotExist)

	// Crash during staging copy: no previous directory, so the partial copy is removed.
	m, root = newManager(t)
	writeGuestFiles(t, filepath.Join(root, "staged"), map[string]string{"boot.raw": "partial"})
	require.NoError(t, m.recoverMacOSRestoreStaging(""))
	_, err = os.Stat(root)
	require.ErrorIs(t, err, os.ErrNotExist)
}

func TestMacOSRestoreRecoveryKeepsBackupWhenInstanceStateIsUnknown(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	const id = "mac-unknown"
	m := &manager{paths: paths.New(t.TempDir())}
	root := filepath.Join(m.paths.DataDir(), "restore-staging", id)
	writeGuestFiles(t, filepath.Join(root, "previous"), map[string]string{"boot.raw": "previous"})
	guests := filepath.Dir(m.paths.InstanceDir(id))
	require.NoError(t, os.MkdirAll(guests, 0755))
	// An unreadable guests directory makes the instance directory's state unknown.
	require.NoError(t, os.Chmod(guests, 0))
	t.Cleanup(func() { _ = os.Chmod(guests, 0755) })

	require.Error(t, m.recoverMacOSRestoreStaging(id))
	require.NoError(t, os.Chmod(guests, 0755))
	data, err := os.ReadFile(filepath.Join(root, "previous", "boot.raw"))
	require.NoError(t, err, "the parked backup must survive an inconclusive check")
	require.Equal(t, "previous", string(data))
}

func TestMacOSRestoreClearsSourceRestartBlock(t *testing.T) {
	restored := StoredMetadata{RestartStatus: restartpolicy.Status{BlockedReason: restartpolicy.BlockedReasonManualStop}}
	source := StoredMetadata{}
	prepareMacOSSnapshotRestoreMetadata(&restored, &source, SnapshotKindStopped, StateRunning)
	require.True(t, restored.RestartStatus.IsZero(), "a running restore must not inherit the source's manual-stop block")

	prepareMacOSSnapshotRestoreMetadata(&restored, &source, SnapshotKindStopped, StateStopped)
	require.Equal(t, restartpolicy.BlockedReasonManualStop, restored.RestartStatus.BlockedReason)
}

func TestMacOSMemoryCompressionRejectsEnabledOnly(t *testing.T) {
	require.NoError(t, validateMacOSMemoryCompression(nil))
	require.NoError(t, validateMacOSMemoryCompression(&snapshot.SnapshotCompressionConfig{Enabled: false}))
	require.ErrorIs(t, validateMacOSMemoryCompression(&snapshot.SnapshotCompressionConfig{Enabled: true}), ErrInvalidRequest)
}

func TestRestoreKeepsCheckpointWhileShimLoads(t *testing.T) {
	m, meta := macOSMemorySnapshotFixture(t)
	writeMacOSMemorySnapshotPayload(t, m, meta.DataDir, &meta.StoredMetadata)
	// A live shim that has not opened its control socket yet.
	shim := exec.Command("sleep", "60")
	require.NoError(t, shim.Start())
	t.Cleanup(func() { shim.Process.Kill(); shim.Wait() })
	prev := findVZShimPIDs
	findVZShimPIDs = func(string) ([]int, error) { return []int{shim.Process.Pid}, nil }
	t.Cleanup(func() { findVZShimPIDs = prev })

	_, err := m.RestoreInstance(context.Background(), meta.Id)
	require.ErrorIs(t, err, ErrInvalidState)
	require.DirExists(t, m.paths.InstanceSnapshotLatest(meta.Id), "the checkpoint is consumed only after the VM resumes")
}

func TestStartRetryFinishesCloneRekey(t *testing.T) {
	m, meta := macOSMemorySnapshotFixture(t)
	meta.HypervisorType = lifecycleNoopHypervisorType
	meta.SkipGuestAgent = false
	meta.MacOS.GuestAgent = true
	meta.IdentityRekeyPending = true
	require.NoError(t, os.WriteFile(meta.SocketPath, nil, 0600))
	lifecycleNoopHypervisorStates.Store(meta.SocketPath, hypervisor.StateRunning)
	t.Cleanup(func() { lifecycleNoopHypervisorStates.Delete(meta.SocketPath) })
	require.NoError(t, m.saveMetadata(meta))

	// The fixture has no guest agent, so the rekey retries until its context ends. The
	// retry must report that failure rather than return a clone still carrying its
	// source's SSH host keys.
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	_, err := m.StartInstance(ctx, meta.Id, StartInstanceRequest{})
	require.Error(t, err)
	reloaded, err := m.loadMetadata(meta.Id)
	require.NoError(t, err)
	require.True(t, reloaded.IdentityRekeyPending, "a failed rekey keeps the flag for the next start")
}

func TestRecoverSourceAfterFailedStandby(t *testing.T) {
	ctx := context.Background()
	standbyErr := errors.New("save metadata")

	// The disk copy predates standby: still Running, with no Standby phase.
	m, meta := macOSMemorySnapshotFixture(t)
	writeMacOSMemorySnapshotPayload(t, m, meta.DataDir, &meta.StoredMetadata)
	started := time.Now().Add(-time.Hour)
	meta.StartedAt = &started
	meta.Phases.Record(phasetracking.PhaseRunning, started)
	require.NoError(t, m.saveMetadata(meta))

	// The record standby reached after stopping the VMM.
	stoppedAt := time.Now()
	standby := cloneStoredMetadata(meta.StoredMetadata)
	standby.StoppedAt = &stoppedAt
	standby.HypervisorProcessIdentity.Clear()
	standby.Phases.Record(phasetracking.PhaseStandby, stoppedAt)

	require.ErrorIs(t, m.recoverSourceAfterFailedStandby(ctx, &Instance{StoredMetadata: standby}, standbyErr), standbyErr)
	inst, err := m.GetInstance(ctx, meta.Id)
	require.NoError(t, err)
	require.Equal(t, StateRunning, inst.State, "the source must not stay in Standby after a failed snapshot")
	disk, err := m.loadMetadata(meta.Id)
	require.NoError(t, err)
	require.Contains(t, disk.Phases.Cumulative, phasetracking.PhaseStandby, "the standby record must reach the disk before the restore reads it")

	// Nil means standby failed before the VMM stopped, so there is nothing to recover.
	m2, meta2 := macOSMemorySnapshotFixture(t)
	require.Equal(t, standbyErr, m2.recoverSourceAfterFailedStandby(ctx, nil, standbyErr))
	_ = meta2
}

func TestRecoverSourceKeepsStandbyWhenRecordCannotBeSaved(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	ctx := context.Background()
	m, meta := macOSMemorySnapshotFixture(t)
	writeMacOSMemorySnapshotPayload(t, m, meta.DataDir, &meta.StoredMetadata)
	require.NoError(t, os.Chmod(meta.DataDir, 0500))
	t.Cleanup(func() { os.Chmod(meta.DataDir, 0700) })

	standbyErr := errors.New("save metadata")
	stoppedAt := time.Now()
	standby := cloneStoredMetadata(meta.StoredMetadata)
	standby.StoppedAt = &stoppedAt
	err := m.recoverSourceAfterFailedStandby(ctx, &Instance{StoredMetadata: standby}, standbyErr)
	require.ErrorIs(t, err, standbyErr)
	require.ErrorContains(t, err, "source left in Standby")
	inst, gerr := m.GetInstance(ctx, meta.Id)
	require.NoError(t, gerr)
	require.Equal(t, StateStandby, inst.State)
}
