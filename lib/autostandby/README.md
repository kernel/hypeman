# Auto Standby

This feature automatically puts a Linux VM into `Standby` after it has stopped serving inbound TCP traffic for a configured amount of time.

## What counts as activity

The feature looks at host-side conntrack state, not ingress configuration and not TAP byte counters.

A VM is considered active when there is at least one tracked TCP flow where:

- the original destination is the VM's private IP
- the VM is the server/responding side of the connection
- the flow is still in an active TCP state

That means:

- inbound client connections keep the VM awake
- replies to outbound guest requests do not keep the VM awake
- same-host clients count by default

## Idle behavior

When the active inbound TCP connection count reaches zero, Hypeman starts an idle timer for that instance.

- if a new inbound TCP connection appears before the timer expires, the timer is cleared
- if the count stays at zero for the full `idle_timeout`, Hypeman places the VM into `Standby`

The timer is in-memory. After a Hypeman restart, idle timers begin from controller startup time instead of being reconstructed from the past. This avoids immediate standby caused only by restarting the control plane.

## Exclusions

Instances can ignore some traffic when deciding whether they are active:

- `ignore_source_cidrs` excludes matching client source ranges
- `ignore_destination_ports` excludes matching VM destination ports

This is intended for probes, internal callers, or ports that should not keep a VM warm.

## Limits

- Linux only
- TCP only
- Wake-on-traffic is not part of this feature

Wake-on-traffic would require a separate host-owned listener or forwarding layer that can accept a connection while the VM is asleep, trigger restore, and then hand traffic through once the VM is running.
