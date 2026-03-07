# Snapshot Store

Manages snapshot metadata persistence and filtering for centrally stored VM snapshots.

## Design Decisions

### Why Separate Package?

**What:** Keep snapshot domain and storage logic in `lib/snapshot`, while `lib/instances` owns VM lifecycle orchestration.

**Why:**
- Snapshot metadata storage is independent from hypervisor runtime control
- Makes snapshot record behavior easier to test in isolation
- Reduces lifecycle-file complexity in `lib/instances/snapshot.go`

### Why Record + Raw Metadata?

**What:** Persist snapshot records as:
- `snapshot` (immutable snapshot fields)
- `stored_metadata` (opaque JSON blob for source instance metadata)

**Why:**
- Snapshot store does not need to understand instance internals
- Instance package can evolve metadata fields without rewriting store logic
- Keeps persistence layer focused on CRUD semantics

## Filesystem Layout

```
{$DATA_DIR}/snapshots/
  {snapshot-id}/
    snapshot.json     # Snapshot record (snapshot + stored_metadata JSON)
    guest/            # Copied guest payload
```

## API Surface

`Store` provides:
- `SaveRecord`
- `LoadRecord`
- `ListRecords`
- `List`
- `Get`
- `Delete`
- `EnsureNameAvailable`

Utilities:
- `DirectoryFileSize` for payload sizing

## Error Model

- `ErrNotFound` when a snapshot record/directory does not exist
- `ErrNameExists` when `EnsureNameAvailable` detects a duplicate snapshot name for a source instance

`lib/instances` maps these to instance-domain errors (`ErrSnapshotNotFound`, `ErrAlreadyExists`) at package boundaries.

## Testing

`store_test.go` covers:
- Save/load/list/delete lifecycle
- Name uniqueness checks
- Filter matching behavior
- Payload file size utility
