# Restart Policy

Restart policy lets Hypeman keep an instance running after the guest program exits and the instance reaches `Stopped`.

This supervises the whole instance, not an individual in-guest process. If the image runs systemd or multiple processes, a restart boots the instance again using the same persisted instance configuration.

## Policies

`never` is the default. Hypeman records exit information, but never restarts the instance.

`on_failure` restarts when the last run failed.

Failure means:

- exit code is nonzero
- the guest was killed by signal or OOM
- the instance stopped unexpectedly and no clean exit code is available

Exit code `0` does not restart under `on_failure`.

`always` restarts after any guest exit, including exit code `0`.

## Manual Stops

Manual stop suppresses restart policy.

When an instance is stopped through the API, Hypeman records an internal suppression marker before shutdown begins. The public instance `state` remains the single lifecycle state; the suppression marker is exposed only as `restart_status.blocked_reason=manual_stop`.

Calling `start` clears manual suppression and retry status.

Deleting an instance removes it entirely and no restart is attempted.

Unexpected guest exit does not set manual suppression. If the instance has a restart policy, the controller may restart it.

## Attempts And Backoff

Each automatic restart waits for `backoff` before another restart attempt is allowed.

`max_attempts` limits consecutive automatic restart attempts. `0` means unlimited attempts.

If `max_attempts` is exceeded, restart policy is blocked for that failure window and `restart_status.blocked_reason` is set to `max_attempts_exceeded`.

If an instance runs for `stable_after`, the consecutive attempt count resets. This prevents old transient failures from permanently consuming the retry budget.

Manual `start` clears blocked restart status and starts a new failure window.

Updating the restart policy clears retry status unless the instance was manually stopped. Changing the policy on a manually stopped instance does not start it; the user must call `start`.

## Lifecycle Behavior

Restart policy only starts instances from `Stopped`.

It does not restore `Standby` instances, wake templates, or act on `Unknown` state.

Automatic restart uses the normal instance start path. Resource validation, network allocation, config disk generation, volumes, GPU attachment, egress policy, and command/env metadata behave the same as a manual start.

The restart keeps the same image, overlay disk, volumes, env, tags, entrypoint, and cmd stored on the instance.

## Status

Instance responses include the configured `restart_policy` and current `restart_status`.

`restart_status.next_attempt_at` is set while waiting for backoff.

`restart_status.attempts` counts consecutive automatic restart attempts in the current failure window.

`restart_status.blocked_reason` explains why no more retries will happen despite a restart policy being configured.

## Non-goals

Restart policy is not a health check.

It does not restart unhealthy-but-running workloads.

It does not supervise individual processes inside the guest.

It does not replace systemd for images that want in-guest service supervision.
