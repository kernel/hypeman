# Egress Proxy (Mock Secret Substitution)

This module provides an optional, default-off networking mode for VM egress.

When enabled for an instance, hypeman does three things:

1. It starts (or reuses) a host-side HTTP/HTTPS MITM proxy bound to the VM bridge gateway.
2. It injects proxy environment variables into the guest (`HTTP_PROXY` / `HTTPS_PROXY`) and installs the proxy CA certificate in the guest trust store.
3. It enforces policy on the host to prevent direct outbound TCP egress from the VM unless traffic is going to the bridge gateway (the proxy), depending on `network.egress.enforcement.mode`.

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

## Security behavior

- Real secret values are persisted in the normal instance `env` metadata, which is already host-side state.
- TLS interception requires guest trust of the proxy CA; hypeman installs this CA in the guest when proxy mode is enabled.
- Egress enforcement is applied per instance TAP device and removed when the instance stops/standbys/deletes.
- Enforcement intentionally targets TCP egress only. DNS/other non-TCP traffic is not rewritten and is not blocked by `all` mode.

## Limits of enforcement

- Header injection is applied to HTTP headers only (not request/response bodies).
- Non-HTTP protocols or custom ports are not rewritten by the MITM layer.
- Plain HTTP requests are not eligible for secret substitution.
