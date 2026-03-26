# Disk Utilization

This package measures the actual on-disk footprint that Hypeman is consuming so operators can answer a different question than the existing allocation metrics.

`hypeman_resources_disk_breakdown_bytes` remains the allocation and provisioned view.  
`hypeman_disk_utilization_bytes` is the actual filesystem utilization view.

The utilization metric reports bytes allocated on disk for these components:

- `images`
- `oci_cache`
- `volumes`
- `rootfs_overlays`
- `volume_overlays`
- `snapshot_uncompressed`
- `snapshot_compressed`
- `snapshot_other`

The measurement is based on filesystem allocated blocks rather than logical file size. That means sparse disks and overlays report the bytes they really occupy on disk, not the size they were provisioned with.

Snapshots are classified by the memory artifact present in `snapshot-latest`:

- `snapshot_compressed` for compressed memory files such as `memory-ranges.zst` or `memory-ranges.lz4`
- `snapshot_uncompressed` for raw `memory-ranges`
- `snapshot_other` when a snapshot directory exists but does not match a recognized memory artifact shape

The full `snapshot-latest` directory is counted once under its classified snapshot component so the metric includes related config and state files, not just the memory artifact itself.

This data is intended to be collected on the monitoring refresh loop and cached before scrape time so `/metrics` stays cheap to serve.
