# JWT secret rotation

`jwt_secret` signs tokens; it and `jwt_verification_secrets` verify them.
Generate a new secret with `openssl rand -hex 32` and store it securely.

## Rotate

Apply each phase with a restart:

| Phase | `jwt_secret` | `jwt_verification_secrets` | Action |
|---|---|---|---|
| Accept both | `<old>` | `["<new>"]` | Deploy everywhere; verify both keys on API and registry, including `/v2/token`. |
| Switch signing | `<new>` | `["<old>"]` | Reissue API/CLI tokens with the same identity and permissions; update every caller. New builder tokens use the new key. |
| Retire old | `<new>` | `[]` | After all callers migrate and old-key builds finish or tokens expire, remove the old key. Verify old tokens fail and new tokens work. |

`/v2/token` does not re-sign credentials. Account for infrequent callers and
year-long installer-issued CLI tokens.

## Caveats

- **Environment overrides YAML:** `JWT_SECRET` sets the signing key;
  `JWT_VERIFICATION_SECRETS='["<old>"]'` sets verification keys as a JSON array.
  Use `'[]'` to clear them; an unset or empty value preserves the YAML list.
- **Restarts can interrupt connections.** Check service process-kill settings
  and `caddy.stop_on_shutdown` (default: `true`).
- **Monitor:** `hypeman_jwt_validations_total{key_role="fallback"}` shows old-key
  validations after switching signing. It is not an authorized-request count;
  zero activity does not prove every caller migrated.
- **Compromised key:** minimize overlap. Anyone holding an accepted key can
  forge tokens until it is removed.
