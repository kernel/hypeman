package instances

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kernel/hypeman/lib/images"
	"github.com/kernel/hypeman/lib/paths"
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
	writeGuestFiles(t, m.paths.InstanceDir(id), map[string]string{"overlay.raw": "after", macOSAuxStorageFile: "aux-after"})
	writeGuestFiles(t, m.paths.SnapshotGuestDir(snap), map[string]string{"overlay.raw": "before", macOSAuxStorageFile: "aux-before", "metadata.json": "stale-metadata"})
	expiresAt := time.Now().Add(time.Hour)
	restored := &metadata{StoredMetadata: StoredMetadata{Id: id, Name: "current-name", ExpiresAt: &expiresAt, Vcpus: 4, Size: 8 << 30}}

	require.NoError(t, m.replaceMacOSInstanceWithSnapshotPayload(snap, restored))
	data, err := os.ReadFile(filepath.Join(m.paths.InstanceDir(id), "overlay.raw"))
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
	writeGuestFiles(t, m.paths.InstanceDir(id), map[string]string{"overlay.raw": "current"})
	require.Error(t, m.replaceMacOSInstanceWithSnapshotPayload("missing-snapshot", &metadata{StoredMetadata: StoredMetadata{Id: id}}))
	data, err = os.ReadFile(filepath.Join(m.paths.InstanceDir(id), "overlay.raw"))
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
	writeGuestFiles(t, filepath.Join(root, "previous"), map[string]string{"overlay.raw": "previous"})
	require.NoError(t, m.recoverMacOSRestoreStaging(id))
	data, err := os.ReadFile(filepath.Join(m.paths.InstanceDir(id), "overlay.raw"))
	require.NoError(t, err)
	require.Equal(t, "previous", string(data))
	_, err = os.Stat(root)
	require.ErrorIs(t, err, os.ErrNotExist)

	// Crash after a completed swap: the instance directory is current, so stale staging goes.
	m, root = newManager(t)
	writeGuestFiles(t, m.paths.InstanceDir(id), map[string]string{"overlay.raw": "restored"})
	writeGuestFiles(t, filepath.Join(root, "previous"), map[string]string{"overlay.raw": "old"})
	require.NoError(t, m.recoverMacOSRestoreStaging(id))
	data, err = os.ReadFile(filepath.Join(m.paths.InstanceDir(id), "overlay.raw"))
	require.NoError(t, err)
	require.Equal(t, "restored", string(data))
	_, err = os.Stat(root)
	require.ErrorIs(t, err, os.ErrNotExist)

	// Crash during staging copy: no previous directory, so the partial copy is removed.
	m, root = newManager(t)
	writeGuestFiles(t, filepath.Join(root, "staged"), map[string]string{"overlay.raw": "partial"})
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
	writeGuestFiles(t, filepath.Join(root, "previous"), map[string]string{"overlay.raw": "previous"})
	guests := filepath.Dir(m.paths.InstanceDir(id))
	require.NoError(t, os.MkdirAll(guests, 0755))
	// An unreadable guests directory makes the instance directory's state unknown.
	require.NoError(t, os.Chmod(guests, 0))
	t.Cleanup(func() { _ = os.Chmod(guests, 0755) })

	require.Error(t, m.recoverMacOSRestoreStaging(id))
	require.NoError(t, os.Chmod(guests, 0755))
	data, err := os.ReadFile(filepath.Join(root, "previous", "overlay.raw"))
	require.NoError(t, err, "the parked backup must survive an inconclusive check")
	require.Equal(t, "previous", string(data))
}
