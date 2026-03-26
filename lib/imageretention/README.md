# Image Retention

This feature automatically deletes cached converted images after they have been unused for a configurable amount of time.

The retention window is server-wide and controlled by:

```yaml
images:
  auto_delete:
    enabled: false
    unused_for: 720h
```

When auto-delete is enabled:

- The server runs a retention sweep on startup and then every minute.
- Only converted cached images under `data_dir/images` are eligible for deletion.
- Shared OCI cache data under `data_dir/system/oci-cache` is not modified.

An image is considered in use if any persisted instance metadata or snapshot record still references it. As long as at least one such reference exists, the image is protected from deletion.

The retention timer starts only after the last persisted reference disappears. At that point the server records `unused_since` for the image digest. Once `unused_since + unused_for` has elapsed, the cached image digest is deleted.

New instance creation clears any stale unused state for the resolved image before the new instance metadata is written. This helps prevent races where an image is being reused right as retention cleanup runs.
