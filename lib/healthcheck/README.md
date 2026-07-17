# Health Checks

Health checks track whether a running workload is healthy without changing the instance lifecycle state.

An instance can be `Running` and `unhealthy` at the same time. `Initializing` still means Hypeman is bringing up the guest and waiting for the existing boot readiness signals. Health checks can run during `Initializing`, but the public status remains `starting` until the instance reaches `Running`.

## Check Types

`none` disables health checks. This is the default.

`http` sends a GET request to the instance network address on the configured port and path. The check succeeds when the response status equals `expected_status`, which defaults to `200`.

`tcp` opens a TCP connection to the instance network address and configured port. The check succeeds when the connection opens.

`exec` runs a command inside the guest after the guest agent is ready. Exit code `0` is healthy. A nonzero exit code, timeout, missing command, or launch error is unhealthy.

## Timing

Each enabled health check has:

- `interval`: how often to run the check after the previous attempt
- `timeout`: the maximum time one check may run
- `start_period`: startup grace period before failures can mark the workload unhealthy
- `failure_threshold`: consecutive failures required to mark unhealthy
- `success_threshold`: consecutive successes required to mark healthy

The defaults are:

```yaml
interval: 10s
timeout: 2s
start_period: 30s
failure_threshold: 3
success_threshold: 1
```

Failures during `start_period` are recorded, but the status remains `starting`. A success during `start_period` can mark the workload `healthy`.

Once healthy, isolated failures do not immediately flip the status. The workload becomes `unhealthy` only after `failure_threshold` consecutive failures.

## Status

Instance responses include `health_check` and `health_status`.

`disabled` means no check is configured.

`unknown` means a check is configured but the instance is not currently active.

`starting` means the instance is initializing, or Hypeman has not yet observed enough successful or failed checks to declare the workload healthy or unhealthy.

`healthy` means the configured check has reached its success threshold.

`unhealthy` means the configured check has reached its failure threshold outside the start period.

Health status includes the last check time, last success time, last failure time, consecutive success and failure counts, and a truncated last error.

## Lifecycle

Health checks do not affect lifecycle state.

Startup remains:

```text
Initializing -> Running
```

With health checks enabled, the health dimension evolves separately:

```text
unknown -> starting -> healthy
unknown -> starting -> unhealthy
```

Stopping, deleting, standing by, or restoring an instance stops active checks. Starting or restoring an instance begins a fresh health window.

## Restart Policy

Health checks do not restart instances by themselves.

When an instance also has `restart_policy.policy=on_failure` or `restart_policy.policy=always`, an `unhealthy` health status becomes a restart-policy failure signal. The restart policy applies its normal backoff, max attempts, manual-stop suppression, and stable-window reset before Hypeman restarts the whole instance.

With `restart_policy.policy=never` or no restart policy, health checks only report status.

Health checks still do not mutate lifecycle state directly. The instance remains `Running` while unhealthy until restart policy chooses to stop and start it.
