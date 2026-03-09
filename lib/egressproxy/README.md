# Egress Proxy (Mock Secret Substitution)

This module provides an optional, default-off networking mode for VM egress.

When enabled for an instance, hypeman does three things:

1. It starts (or reuses) a host-side HTTP/HTTPS MITM proxy bound to the VM bridge gateway.
2. It injects proxy environment variables into the guest (`HTTP_PROXY` / `HTTPS_PROXY`) and installs the proxy CA certificate in the guest trust store.
3. It enforces policy on the host to prevent direct outbound TCP egress from the VM unless traffic is going to the bridge gateway (the proxy), depending on `egress_proxy.enforcement_mode`.

## Secret substitution flow

- API callers provide real secret values in instance `env`.
- Per instance, `egress_proxy.mock_env_vars` lists which env var names should be mocked.
- Per instance, `egress_proxy.mock_env_var_domains` can optionally restrict each mocked env var to specific destination hosts:
  - Exact host: `api.openai.com`
  - Single-level wildcard: `*.openai.com`
  - If a mocked env var is omitted from this map, substitution is allowed for all HTTPS destinations.
- Per instance, `egress_proxy.enforcement_mode` controls host-side direct egress blocking:
  - `all` (default when proxy is enabled): reject direct non-proxy TCP egress from the VM TAP interface.
  - `http_https_only`: reject direct TCP egress only on destination ports `80` and `443`.
- Inside the VM, each listed env var is rewritten to `mock-<ENV_VAR_NAME>` (for example `mock-OUTBOUND_OPENAI_KEY`).
- Substitution is applied to HTTPS requests only after MITM decryption.
- For HTTPS egress, the proxy validates upstream TLS certificates with the host trust store before forwarding.
- The proxy scans each HTTP header value and replaces configured mock values with real values only when the verified destination host matches the env var's allowlist (if configured).
- The modified request is then forwarded upstream.

This keeps real secrets out of the VM while still allowing authenticated egress requests.

## Security behavior

- Real secret values are persisted in the normal instance `env` metadata, which is already host-side state.
- TLS interception requires guest trust of the proxy CA; hypeman installs this CA in the guest when proxy mode is enabled.
- Egress enforcement is applied per instance TAP device and removed when the instance stops/standbys/deletes.
- Enforcement intentionally targets TCP egress only. DNS/other non-TCP traffic is not rewritten and is not blocked by `all` mode.

## Limits of enforcement

- Header replacement is applied to HTTP headers only (not request/response bodies).
- Non-HTTP protocols or custom ports are not rewritten by the MITM layer.
- Plain HTTP requests are not eligible for secret substitution.
