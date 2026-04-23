# OCI Cache GC

Mark-and-sweep garbage collector for the shared OCI cache at
`data_dir/system/oci-cache`.

The cache is populated every time an image is pulled or pushed and was
previously write-only: nothing ever removed layer, config, or manifest
blobs, so the cache grew unbounded. This collector reclaims the space
used by manifests and layers that are no longer referenced from
`index.json`.

## Configuration

```yaml
images:
  oci_cache_gc:
    enabled: false
    interval: 1h
    min_blob_age: 1h
```

When enabled, the server runs one pass immediately and then every
`interval` until shutdown.

## Algorithm

1. **Mark.** Read `index.json` and walk every referenced descriptor. For
   each manifest or manifest-index blob we descend into its `config`,
   `layers`, `manifests`, and `subject` references. The set of visited
   digests is the live set.
2. **Sweep.** List `blobs/sha256/`. Delete every file whose name is a
   valid 64-char hex digest, is absent from the live set, and whose
   `mtime` is older than `min_blob_age`.

Blobs that are referenced but unparseable are kept as opaque leaves; the
collector never deletes a blob it cannot prove is dead.

## Concurrency

Pulls (`layout.AppendImage`) and pushes (`BlobStore.Put`) write blobs
before updating `index.json`. During that window a blob exists on disk
but is not yet in the live set. `min_blob_age` is the grace period that
protects these in-flight writes — it should comfortably exceed the time
it takes to pull the largest image in your environment.

Temporary files (`<digest>.tmp` used by `BlobStore.Put`) are ignored
entirely because they do not match the blob filename pattern.

## Metrics

| Metric | Type | Description |
| ------ | ---- | ----------- |
| `hypeman_oci_cache_gc_sweeps_total` | counter | Sweeps, tagged by status |
| `hypeman_oci_cache_gc_sweep_duration_seconds` | histogram | Sweep duration |
| `hypeman_oci_cache_gc_deleted_blobs_total` | counter | Blobs deleted |
| `hypeman_oci_cache_gc_deleted_bytes_total` | counter | Bytes reclaimed |
