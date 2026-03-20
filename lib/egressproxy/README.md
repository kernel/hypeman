# Egress Proxy (Mock Secret Substitution)

This module provides an optional, default-off networking mode for VM egress.

When enabled for an instance, hypeman does three things:

1. It starts (or reuses) a host-side HTTP/HTTPS MITM proxy bound to the VM bridge gateway.
2. It injects proxy environment variables into the guest (`HTTP_PROXY` / `HTTPS_PROXY`) and installs the proxy CA certificate in the guest trust store.
3. It enforces policy on the host to prevent direct outbound TCP egress from the VM unless traffic is going to the bridge gateway (the proxy), depending on `network.egress.enforcement.mode`.

## How the feature works

At a high level, the feature separates what the guest sees from what the host uses for outbound authentication:

- The guest gets normal proxy configuration plus mock credential values.
- The host keeps the real credential values in instance metadata.
- The host-side proxy injects the real values into outbound HTTPS headers only when the configured policy matches.

That means the VM can make authenticated outbound requests without ever receiving the real secret material directly.

## Secret substitution flow

- API callers provide real secret values in instance `env`.
- Per instance, `credentials` defines host-managed credential brokering policies keyed by guest-visible credential name.
- Each credential policy uses:
  - `source.env` for the real value source in host env.
  - `inject[*].hosts` to optionally restrict destination hosts:
    - Exact host: `api.openai.com`
    - Single-level wildcard: `*.openai.com`
    - If omitted, injection is allowed for all destinations.
  - `inject[*].as` to define the header/format template shape.
- Per instance, `network.egress.enforcement.mode` controls host-side direct egress blocking:
  - `all` (default when proxy is enabled): reject direct non-proxy TCP egress from the VM TAP interface.
  - `http_https_only`: reject direct TCP egress only on destination ports `80` and `443`.
- Inside the VM, each credential key is rewritten to `mock-<CREDENTIAL_NAME>` (for example `mock-OUTBOUND_OPENAI_KEY`).
- Header injection is applied to HTTPS requests only after MITM decryption.
- For HTTPS egress, the proxy validates upstream TLS certificates with the host trust store before forwarding.
- The proxy materializes the configured `inject[*].as.header` using `inject[*].as.format` with the real value only when the verified destination host matches the credential allowlist (if configured).
- The modified request is then forwarded upstream.

This keeps real secrets out of the VM while still allowing authenticated egress requests.

## Credential rotation via instance update

The feature also supports rotating real credential values without restarting the VM.

- `PATCH /instances/{id}` accepts updates to env keys that are already referenced by existing credential `source.env` bindings.
- The request updates the host-side stored value for that credential source.
- If the instance is currently registered with the egress proxy, hypeman recompiles the proxy's header injection rules using the new real value.
- The guest-visible env value does not change: inside the VM, the credential still appears as `mock-<CREDENTIAL_NAME>`.
- New outbound HTTPS requests start using the rotated value after the update succeeds.

Operationally, this is intended for key rotation, revocation/reissue flows, and similar secret lifecycle events where you want host-side outbound auth behavior to change immediately without a guest reboot.

### Update safety behavior

- The update path only accepts env keys already bound by the instance's credential policy.
- If proxy rule recompilation fails, the running instance keeps using the old value.
- If runtime rules are updated but metadata persistence fails, hypeman rolls the proxy rules back to the previous value before returning an error.
- Invalid or unreadable metadata is treated as an internal failure, not as a synthetic "instance not found" result.

## Security behavior

- Real secret values are persisted in the normal instance `env` metadata, which is already host-side state.
- TLS interception requires guest trust of the proxy CA; hypeman installs this CA in the guest when proxy mode is enabled.
- Egress enforcement is applied per instance TAP device and removed when the instance stops/standbys/deletes.
- Enforcement intentionally targets TCP egress only. DNS/other non-TCP traffic is not rewritten and is not blocked by `all` mode.

## Limits of enforcement

- Header injection is applied to HTTP headers only (not request/response bodies).
- Non-HTTP protocols or custom ports are not rewritten by the MITM layer.
- Plain HTTP requests are not eligible for secret substitution.

## Observability

This feature exposes operator-facing logs, traces, and metrics for both the control plane and the proxy data plane.

### Logs

- Egress proxy logs use the `EGRESS` logging subsystem.
- Control-plane actions such as register, unregister, and rule update include `instance_id` so they can be correlated with per-instance logs.
- Upstream proxy failures are logged with low-cardinality fields such as `protocol` and whether header injection occurred (`injected=true/false`).
- When request trace context is available, logs include `trace_id` and `span_id`.

In normal operation, the important things to watch for are:

- repeated "failed to configure egress proxy" errors during create, start, or restore
- repeated "failed to update egress proxy rules" errors during credential rotation
- repeated upstream proxy failure warnings, especially after a credential rotation or policy rollout

### Tracing

The feature adds child spans for control-plane operations, including:

- `MaybeRegisterEgressProxy`
- `EgressProxy.RegisterInstance`
- `EgressProxy.UpdateInstanceRules`
- `EgressProxy.UnregisterInstance`

These spans include attributes such as:

- `operation`
- `proxy_enabled`
- `enforcement_mode`
- `inject_rule_count`
- `result`

This makes it possible to distinguish failures in proxy registration, runtime rule update, and teardown from the broader instance lifecycle span that triggered them.

### Metrics

Control-plane metrics:

- `hypeman_egress_proxy_registrations_total{operation,result,enforcement_mode}`
- `hypeman_egress_proxy_rule_updates_total{result}`
- `hypeman_egress_proxy_registered_instances_total`
- `hypeman_egress_proxy_control_plane_duration_seconds{operation,result}`

Data-plane metrics:

- `hypeman_egress_proxy_requests_total{protocol,result,injected}`
- `hypeman_egress_proxy_upstream_duration_seconds{protocol,result}`
- `hypeman_egress_proxy_upstream_failures_total{protocol}`

These labels are intentionally low-cardinality. In particular, destination host is not used as a metric label.

### What operators should look for

- A rise in `hypeman_egress_proxy_registrations_total{result="error"}` usually means create/start/restore flows are failing to attach egress mediation correctly.
- A rise in `hypeman_egress_proxy_rule_updates_total{result="error"}` means key rotation requests are being rejected or failing to apply.
- `hypeman_egress_proxy_registered_instances_total` should roughly match the number of running instances that currently have `network.egress.enabled=true`.
- A rise in `hypeman_egress_proxy_upstream_failures_total` or a latency increase in `hypeman_egress_proxy_upstream_duration_seconds` usually points to upstream reachability, TLS trust, or destination-side issues rather than guest boot problems.
