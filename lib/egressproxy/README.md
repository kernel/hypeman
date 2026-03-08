# Egress Proxy (Mock Secret Substitution)

This module provides an optional, default-off networking mode for VM egress.

When enabled for an instance, hypeman does three things:

1. It starts (or reuses) a host-side HTTP/HTTPS MITM proxy bound to the VM bridge gateway.
2. It injects proxy environment variables into the guest (`HTTP_PROXY` / `HTTPS_PROXY`) and installs the proxy CA certificate in the guest trust store.
3. It enforces policy on the host so direct outbound TCP traffic on ports `80` and `443` from that VM's TAP interface is rejected unless it is going to the bridge gateway (the proxy).

## Secret substitution flow

- API callers provide real secret values in instance `env`.
- Per instance, `egress_proxy.mock_env_vars` lists which env var names should be mocked.
- Inside the VM, each listed env var is rewritten to `mock-<ENV_VAR_NAME>` (for example `mock-OUTBOUND_OPENAI_KEY`).
- For each outbound HTTP request (including HTTPS requests after MITM decryption), the proxy scans every HTTP header value.
- Any occurrence of a configured mock value is replaced with the real value loaded from the instance's stored `env`.
- The modified request is then forwarded upstream.

This keeps real secrets out of the VM while still allowing authenticated egress requests.

## Security behavior

- Real secret values are persisted in the normal instance `env` metadata, which is already host-side state.
- TLS interception requires guest trust of the proxy CA; hypeman installs this CA in the guest when proxy mode is enabled.
- Egress enforcement is applied per instance TAP device and removed when the instance stops/standbys/deletes.

## Limits of enforcement

- Enforcement currently targets HTTP/HTTPS default ports (`80` and `443`).
- Non-HTTP protocols or custom ports are not rewritten.
- Header replacement is applied to HTTP headers only (not request/response bodies).
