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

## How Measurement Works

The measurement is done by walking only the known Hypeman storage roots instead of scanning the whole data filesystem.

- `images` is measured from exported image disk files such as `rootfs.erofs` or `rootfs.ext4`
- `oci_cache` is measured from the OCI cache tree
- `volumes` is measured from volume `data.raw` files
- `rootfs_overlays` is measured from each guest `overlay.raw`
- `volume_overlays` is measured from each guest `vol-overlays/*.raw`
- snapshots are measured from each guest `snapshots/snapshot-latest` directory

The measurement is based on filesystem allocated blocks rather than logical file size. That means sparse disks and overlays report the bytes they really occupy on disk, not the size they were provisioned with.

Concretely, the collector reads filesystem metadata and uses the allocated block count for each file or directory entry, so the result reflects actual blocks consumed on disk. Directory walks are limited to Hypeman-managed paths that are already known from the data layout.

Snapshots are classified by the memory artifact present in `snapshot-latest`:

- `snapshot_compressed` for compressed memory files such as `memory-ranges.zst` or `memory-ranges.lz4`
- `snapshot_uncompressed` for raw `memory-ranges`
- `snapshot_other` when a snapshot directory exists but does not match a recognized memory artifact shape

The full `snapshot-latest` directory is counted once under its classified snapshot component so the metric includes related config and state files, not just the memory artifact itself.

## How It Is Stored For Metrics

This package returns a per-component breakdown to the resource monitoring refresh loop. The refresh loop stores the latest measured values in the in-memory monitoring snapshot, and the Prometheus callback only reads that cached snapshot.

That means:

- the expensive filesystem work happens on the refresh interval
- the `/metrics` scrape path does not walk the filesystem
- each scrape simply emits the latest cached component values

## Efficiency Expectations

This should be efficient enough for the current design because it avoids whole-filesystem scans and avoids any disk walking during Prometheus scrapes.

The main cost is the periodic walk over Hypeman-managed storage roots. That is still proportional to the number of tracked files and snapshot directories, so it is not free, but it is bounded and predictable. For v1, this is a good tradeoff: correct sparse-file accounting, accurate snapshot classification, cheap scrapes, and much lower complexity than a change-tracking cache.
