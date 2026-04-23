# Image Retention

This feature automatically deletes cached converted images after they have been unused for a configurable amount of time.

The retention window is server-wide and controlled by:

```yaml
images:
  auto_delete:
    enabled: false
    unused_for: 720h
    allowed: []
```

When auto-delete is enabled:

- The server runs a retention sweep on startup and then every minute.
- Only converted cached images under `data_dir/images` are eligible for deletion.
- Shared OCI cache data under `data_dir/system/oci-cache` is not modified by this feature; see `lib/ocicachegc` for a separate mark-and-sweep collector that reclaims orphaned blobs from that directory.
- An image repository must also match at least one `allowed` pattern before any retention state is recorded or deletion is attempted.

An image is considered in use if any persisted instance metadata or snapshot record still references it. As long as at least one such reference exists, the image is protected from deletion.

The retention timer starts only after the last persisted reference disappears and the image repository is allowed by policy. At that point the server records `unused_since` for the image digest. Once `unused_since + unused_for` has elapsed, the cached image digest is deleted.

Allow-list patterns match normalized repository names such as `docker.io/library/alpine`. A lone `*` pattern allows deletion for every repository.

New instance creation clears any stale unused state for the resolved image before the new instance metadata is written. This helps prevent races where an image is being reused right as retention cleanup runs.
