# Auto Standby

This feature automatically puts a Linux VM into `Standby` after it has stopped serving inbound TCP traffic for a configured amount of time.

## What counts as activity

The feature looks at host-side conntrack state, not ingress configuration and not TAP byte counters.

A VM is considered active when there is at least one tracked TCP flow where:

- the original destination is the VM's private IP
- the VM is the server/responding side of the connection
- the flow is currently tracked as live by conntrack

That means:

- inbound client connections keep the VM awake
- replies to outbound guest requests do not keep the VM awake
- same-host clients count by default

## Idle behavior

Hypeman seeds its controller from a conntrack snapshot on startup, then keeps state current with conntrack netlink events.

- new inbound TCP flows are tracked from conntrack `NEW` events
- TCP teardown is treated as inactivity once conntrack reports a terminal state or the flow disappears
- connections that were already open when Hypeman started are reconciled against fresh conntrack snapshots until they drain, so restart-seeded traffic can still age out correctly
- Hypeman also performs a full snapshot sync every 5 minutes by default as a low-frequency consistency check; the controller interval is configurable

When the active inbound TCP connection count reaches zero, Hypeman starts an idle timer for that instance.

- if a new inbound TCP connection appears before the timer expires, the timer is cleared
- if the count stays at zero for the full `idle_timeout`, Hypeman places the VM into `Standby`

Standby operations execute on background workers so the controller keeps processing
conntrack and instance lifecycle events while snapshots are written. Concurrency is
capped by `auto_standby.max_concurrent` in the server config (default 16); demand
above the cap queues. Inbound activity observed while an instance's standby is
queued cancels that attempt.

The idle timestamps are also persisted in instance metadata.

- if Hypeman restarts and a startup conntrack snapshot shows current inbound connections, the instance is treated as active immediately and any old idle countdown is cleared
- if Hypeman restarts and the snapshot shows zero current inbound connections, Hypeman resumes the persisted idle countdown

This keeps the restart behavior conservative about current traffic while still allowing long idle windows to carry across control-plane restarts.

## Exclusions

Instances can ignore some traffic when deciding whether they are active:

- `ignore_source_cidrs` excludes matching client source ranges
- `ignore_destination_ports` excludes matching VM destination ports

This is intended for probes, internal callers, or ports that should not keep a VM warm.

## Limits

- Linux only
- TCP only
- IPv4 conntrack only
- Wake-on-traffic is not part of this feature

## Status endpoint

Hypeman exposes a diagnostic status endpoint for each instance that reports:

- whether auto-standby is supported, configured, enabled, and currently eligible
- how many qualifying inbound TCP connections are currently keeping the VM awake
- the current idle timer timestamps and next planned standby time
- the current controller reason, such as active inbound traffic, countdown still running, or observer failure

Wake-on-traffic would require a separate host-owned listener or forwarding layer that can accept a connection while the VM is asleep, trigger restore, and then hand traffic through once the VM is running.
