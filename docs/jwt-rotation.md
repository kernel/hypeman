# JWT secret rotation

Hypeman signs builder registry tokens with `jwt_secret` (`JWT_SECRET`) and accepts
that key plus any `jwt_verification_secrets`. The additional keys are only used
for verification, never signing. `hypeman-token` also continues to sign with
`jwt_secret` / `JWT_SECRET`.

All keys grant the same signing authority. Verification-only describes how
Hypeman uses a key, not a restriction on tokens someone holding it can create.
Keep every accepted key secret. If a signing key is compromised, minimize the
overlap: forged tokens remain valid until that key is removed.

## Configuration

```yaml
jwt_secret: "<current-signing-secret>"
jwt_verification_secrets:
  - "<previous-or-next-secret>"
```

The signing secret is still required. Empty verification-key entries are rejected.
Existing single-key configurations work unchanged; tokens do not need a `kid`.
Expiration, not-before, revocation, and permission checks still apply.

For environment-based configuration, supply a JSON array, not a comma-separated
string:

```sh
JWT_VERIFICATION_SECRETS='["<previous-or-next-secret>"]'
```

A nonempty environment value overrides the YAML list. Use `[]` to clear it;
an unset or empty environment variable leaves the YAML value in effect. Do not
concatenate keys into `JWT_SECRET`: its entire value is one signing key.

## Rotation sequence

Generate a new random secret, for example with `openssl rand -hex 32`, and store
it using your normal secret-management process. Do not put real keys in source
control, logs, or shell history.

1. **Accept both keys everywhere, still signing with the old key.**

   ```yaml
   jwt_secret: "<old>"
   jwt_verification_secrets: ["<new>"]
   ```

   Roll this configuration out to every host that will receive the new tokens.
   Verify both old- and new-signed credentials work on the API and registry,
   including `/v2/token`. Do not migrate callers until every verifier accepts
   the new key.

2. **Switch signing, retaining old-key verification.**

   ```yaml
   jwt_secret: "<new>"
   jwt_verification_secrets: ["<old>"]
   ```

   New builder registry tokens use the new key. Reissue API and CLI credentials
   using the new key, preserving each caller's identity and permissions, then
   update every caller. Check for stale `JWT_SECRET` environment overrides when
   using `hypeman-token`; they take precedence over its YAML configuration.
   `/v2/token` returns the submitted credential unchanged; it does not exchange
   old-signed tokens for new-signed ones.

3. **Retire the old key.**

   Confirm all callers have migrated and outstanding old-key builder tokens have
   expired or their builds have finished. Account for infrequent callers and
   long-lived CLI tokens; the installer issues CLI tokens valid for a year.

   ```yaml
   jwt_secret: "<new>"
   jwt_verification_secrets: []
   ```

   Remove any environment override that still accepts the old key, or set
   `JWT_VERIFICATION_SECRETS='[]'`. Roll out the configuration, verify new tokens
   work and old tokens fail, then retire the old secret from storage.

During the overlap, either signing key can be restored without breaking clients
as long as both remain accepted. After retirement, reverting to a configuration
that accepts the old key restores its authority too.

## Observability and restarts

`hypeman_jwt_validations_total{key_role="primary"}` and
`hypeman_jwt_validations_total{key_role="fallback"}` count successful JWT
signature and claims validations. Once the primary key is new and the only
fallback is old, fallback activity indicates continued use of the old key.
These are not unique request counts or authorization decisions: registry auth
may validate a token more than once before enforcing its type and scope.
No secrets, token contents, or key fingerprints are recorded in the metric.
A quiet counter alone cannot prove that an infrequent caller has migrated.

Configuration changes require a Hypeman restart; there is no hot reload.
Dual-key acceptance avoids authentication failures during staggered credential
updates, not restart-related connection interruptions. Check the deployed
service's process-kill policy and `caddy.stop_on_shutdown` before restarting.
The built-in Caddy shutdown default is `true`; explicitly configure `false` if
Caddy should remain running across Hypeman restarts. Drain or retry affected
operations as appropriate for your deployment.
