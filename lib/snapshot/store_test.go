package snapshot

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kernel/hypeman/lib/hypervisor"
	"github.com/kernel/hypeman/lib/paths"
	"github.com/stretchr/testify/require"
)

func TestStoreSaveLoadListDelete(t *testing.T) {
	t.Parallel()

	p := paths.New(t.TempDir())
	store := NewStore(p)

	record := &Record{
		Snapshot: Snapshot{
			Id:               "snap1",
			Name:             "baseline",
			Kind:             SnapshotKindStandby,
			SourceInstanceID: "inst1",
			SourceName:       "vm1",
			SourceHypervisor: hypervisor.TypeQEMU,
			CreatedAt:        time.Now().UTC().Truncate(time.Second),
			SizeBytes:        1234,
		},
		StoredMetadata: json.RawMessage(`{"id":"inst1","name":"vm1"}`),
	}

	require.NoError(t, store.SaveRecord(record))

	got, err := store.LoadRecord(record.Snapshot.Id)
	require.NoError(t, err)
	require.Equal(t, record.Snapshot.Id, got.Snapshot.Id)
	require.JSONEq(t, string(record.StoredMetadata), string(got.StoredMetadata))

	listed, err := store.List(nil)
	require.NoError(t, err)
	require.Len(t, listed, 1)
	require.Equal(t, record.Snapshot.Id, listed[0].Id)

	require.NoError(t, store.Delete(record.Snapshot.Id))
	_, err = store.LoadRecord(record.Snapshot.Id)
	require.Error(t, err)
	require.True(t, errors.Is(err, ErrNotFound))
}

func TestStoreEnsureNameAvailable(t *testing.T) {
	t.Parallel()

	p := paths.New(t.TempDir())
	store := NewStore(p)

	require.NoError(t, store.SaveRecord(&Record{
		Snapshot: Snapshot{
			Id:               "snap1",
			Name:             "baseline",
			Kind:             SnapshotKindStandby,
			SourceInstanceID: "inst1",
		},
		StoredMetadata: json.RawMessage(`{}`),
	}))

	require.NoError(t, store.EnsureNameAvailable("inst1", "different"))
	require.NoError(t, store.EnsureNameAvailable("inst2", "baseline"))

	err := store.EnsureNameAvailable("inst1", "baseline")
	require.Error(t, err)
	require.True(t, errors.Is(err, ErrNameExists))
}

func TestListSnapshotsFilterMatches(t *testing.T) {
	t.Parallel()

	kind := SnapshotKindStandby
	sourceID := "inst1"
	name := "snap"
	filter := &ListSnapshotsFilter{
		SourceInstanceID: &sourceID,
		Kind:             &kind,
		Name:             &name,
	}

	require.True(t, filter.Matches(&Snapshot{
		SourceInstanceID: "inst1",
		Kind:             SnapshotKindStandby,
		Name:             "snap",
	}))
	require.False(t, filter.Matches(&Snapshot{
		SourceInstanceID: "inst2",
		Kind:             SnapshotKindStandby,
		Name:             "snap",
	}))
}

func TestDirectoryFileSize(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "a.bin"), []byte("abc"), 0644))
	require.NoError(t, os.MkdirAll(filepath.Join(root, "nested"), 0755))
	require.NoError(t, os.WriteFile(filepath.Join(root, "nested", "b.bin"), []byte("12345"), 0644))

	size, err := DirectoryFileSize(root)
	require.NoError(t, err)
	require.Equal(t, int64(8), size)
}

func TestResolveTargetState(t *testing.T) {
	t.Parallel()

	state, err := ResolveTargetState(SnapshotKindStandby, "")
	require.NoError(t, err)
	require.Equal(t, StateRunning, state)

	state, err = ResolveTargetState(SnapshotKindStopped, "")
	require.NoError(t, err)
	require.Equal(t, StateStopped, state)

	state, err = ResolveTargetState(SnapshotKindStandby, StateStandby)
	require.NoError(t, err)
	require.Equal(t, StateStandby, state)

	_, err = ResolveTargetState(SnapshotKindStopped, StateStandby)
	require.Error(t, err)
}
