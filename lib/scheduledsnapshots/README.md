# Scheduled Snapshots

This feature lets users attach a periodic snapshot policy to an instance.

- A schedule runs on a fixed interval (for example `1h` or `24h`).
- Each run creates a normal instance snapshot (`Standby` or `Stopped`).
- If `kind` is omitted, the schedule defaults to `Standby`.
- `Standby` captures from a running instance by doing a brief pause/resume cycle during the snapshot operation.
- Each schedule stores runtime status (`next_run_at`, `last_run_at`, `last_snapshot_id`, `last_error`).

Retention cleanup is required per schedule and can use either or both:

- `max_count`: keep only the newest N scheduled snapshots for that instance.
- `max_age`: delete scheduled snapshots older than a duration.

Cleanup only applies to snapshots created by that schedule for that same source instance.
Manual snapshots are never deleted by scheduled retention.
