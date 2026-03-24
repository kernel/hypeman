# Scheduled Snapshots

This feature lets users attach a periodic snapshot policy to an instance.

- A schedule runs on a fixed interval (for example `1h` or `24h`).
- New schedules establish cadence at `now + interval + deterministic jitter` derived from the instance ID.
- Updating only retention, metadata, or `name_prefix` preserves `next_run_at`; changing `interval` establishes a new cadence.
- Each run auto-selects snapshot behavior from current instance state:
  - `Running` or `Standby` source -> `Standby` snapshot
  - `Stopped` source -> `Stopped` snapshot
- For running sources, scheduled capture includes a brief pause/resume cycle during snapshot creation.
- The minimum interval is `1m`, but running/standby captures briefly pause/resume the guest, so larger intervals are recommended for heavier workloads.
- Each schedule stores runtime status (`next_run_at`, `last_run_at`, `last_snapshot_id`, `last_error`).

Retention cleanup is required per schedule and can use either or both:

- `max_count`: keep only the newest N scheduled snapshots for that instance.
- `max_age`: delete scheduled snapshots older than a duration.

Cleanup only applies to snapshots created by that schedule for that same source instance.
Manual snapshots are never deleted by scheduled retention.

If the source instance is deleted, the schedule is kept so retention can continue cleaning previously created scheduled snapshots.
Once no scheduled snapshots remain for that deleted instance, the scheduler removes the stale schedule automatically.
