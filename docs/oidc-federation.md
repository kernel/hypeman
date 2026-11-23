# OIDC federation

A workload that already holds an OIDC identity token can trade it for a short-lived Hypeman API
token, so no long-lived token has to be stored with the workload. GitHub Actions, AWS, Google
Cloud, Microsoft Entra, Kubernetes service accounts and any other OIDC issuer you configure are
supported.

Nothing is trusted by default. You configure an **issuer** (who signs the identity tokens) and a
**federation rule** (which workload of that issuer is accepted, and what it receives) in the `oidc`
block of the server configuration, then restart Hypeman.

## Configure

```yaml
oidc:
  issuers:
    - name: github-actions
      preset_type: github_actions
  federation_rules:
    - name: ci
      issuer: github-actions
      expected_audience: https://hypeman.example.com
      match:
        type: github_actions
        repository_id: "123456789"
        repository_owner_id: "987654321"
        workflow: .github/workflows/deploy.yml
        ref: refs/heads/main
        event_names: [push, workflow_dispatch]
      authorization:
        permissions: [image:read, image:write]
        max_token_ttl_seconds: 300
```

Unknown keys inside `oidc` are rejected, and an invalid block stops the server from starting.
`OIDC` in the environment is one JSON document with the same shape; it replaces the YAML block as
a whole.

### Issuers

| Key | Meaning |
|---|---|
| `name` | Letters, digits and hyphens. Rules refer to the issuer by it. |
| `preset_type` | `github_actions`, `aws`, `google_cloud`, `microsoft_entra`, `kubernetes` or `custom_oidc`. GitHub, Google and Entra issuers are recognized by their URL and must use their own preset. Defaults to the preset the URL belongs to, or `custom_oidc`. |
| `issuer_url` | The exact `iss` value of the issuer's tokens. Implied for `github_actions` and `google_cloud`. |
| `jwks.type` | `discovery` (default) reads `<issuer_url>/.well-known/openid-configuration`. `explicit_url` reads `jwks.url`. `inline` uses the public JWKs listed in `jwks.keys`. |
| `jwks.discovery_base_url` | Fetch the discovery document from here instead of the issuer URL. |
| `jwks.ca_cert_pem` | CA certificates that replace the system roots for the key endpoint. |
| `token_validation.max_lifetime_seconds` | Longest `exp - iat` accepted. Default 3600 (7500 for Entra). |
| `token_validation.replay_protection` | `single_use` or `allow_reuse`. See [Replay](#replay). |

Keys are fetched over HTTPS from public addresses only, without following redirects. For an
issuer that is not reachable from the public internet, list its public keys with `jwks.type:
inline`.

### Federation rules

| Key | Meaning |
|---|---|
| `name` | Letters, digits and hyphens. Callers name the rule in `federation_rule_id`. |
| `issuer` | The issuer's `name`. |
| `expected_audience` | The single `aud` value the identity token must carry. Use a value specific to this Hypeman server, such as its API URL. |
| `match` | Conditions on the token's claims; all must hold. `match.type` must be the issuer's preset. |
| `authorization.permissions` | The [scopes](../lib/scopes/README.md) the issued token carries. Required, and `*` is not accepted. |
| `authorization.max_token_ttl_seconds` | Lifetime of the issued token. Default 300, at most 600. |

What `match` must contain depends on the preset:

| Preset | Required | Also available |
|---|---|---|
| `github_actions` | `repository_id` and `repository_owner_id` (numeric IDs, which survive a rename), one of `workflow`, `workflow_ref` or `job_workflow_ref`, and `ref` or `environment` | `event_names` (default `push` and `workflow_dispatch`), `claims` |
| `google_cloud` | `subject` (the numeric service account ID) | `claims` |
| `microsoft_entra` | `subject`, or a `/oid`, `/appid` or `/azp` claim | `claims`. The tenant comes from the issuer URL. Tokens with delegated scopes are refused. |
| `kubernetes` | `namespace` and `service_account` | `claims` |
| `aws`, `custom_oidc` | `subject`, `subject_prefix` or at least one entry in `claims` | |

`claims` maps a [JSON Pointer](https://www.rfc-editor.org/rfc/rfc6901) to the text a claim must
equal, for example `/runner_environment: github-hosted`. Booleans and numbers compare by their JSON
text. Objects and arrays never match; address an array element by index.

A scheduled GitHub Actions workflow needs `event_names: [schedule]`. `workflow` is the path of the
workflow file in the repository named by `repository_id`, at the token's `ref`. For a reusable
workflow, pin the called workflow with `job_workflow_ref`.

## Exchange a token

Send a form-encoded request with no `Authorization` header:

```bash
curl -sS https://hypeman.example.com/oauth/token \
  --data-urlencode grant_type=urn:ietf:params:oauth:grant-type:jwt-bearer \
  --data-urlencode assertion="$IDENTITY_TOKEN" \
  --data-urlencode federation_rule_id=ci
```

```json
{
  "access_token": "eyJ...",
  "token_type": "Bearer",
  "expires_in": 300,
  "scope": "image:read image:write",
  "principal": "federation:ci",
  "federation_rule_id": "ci"
}
```

Use `access_token` as the API token (`HYPEMAN_API_KEY`). There is no refresh token: request a new
identity token and exchange again. The issued token never outlives the identity token it was
exchanged for.

In GitHub Actions, a job with `permissions: id-token: write` requests the identity token with the
rule's audience:

```bash
IDENTITY_TOKEN=$(curl -sS -H "Authorization: Bearer $ACTIONS_ID_TOKEN_REQUEST_TOKEN" \
  "$ACTIONS_ID_TOKEN_REQUEST_URL&audience=https://hypeman.example.com" | jq -r .value)
```

Errors are JSON with `error` and `error_description`:

| `error` | Status | Meaning |
|---|---|---|
| `invalid_request` | 400 | The form is malformed, repeats a parameter or has no `grant_type`. For the supported grant type with federation configured: an unknown or missing parameter, or an `Authorization` header |
| `unsupported_grant_type` | 400 | The form is well-formed, but it names another grant type or no federation rule is configured |
| `invalid_grant` | 400 | The rule or the identity token was not accepted. The response does not say why. |
| `temporarily_unavailable` | 503 | No token can be issued right now: the issuer's keys could not be loaded, the record of spent tokens could not be written or is full, or the server started without that record (see [Replay](#replay)). Retry with a fresh identity token. |

The reason for a refusal is in the server log, in the `oidc assertion exchange` line for the
request.

## Replay

With `single_use`, an identity token can be exchanged once. Its ID (`jti`, or `uti` for Entra) is
remembered until it expires. GitHub Actions and AWS are always single-use. Google identity tokens
carry no ID and always allow reuse. Kubernetes allows reuse by default, because a pod reads the
same projected token until it is rotated; set `single_use` when every exchange fetches a fresh
token.

The record of spent tokens is kept in memory and in the file `oidc/spent-assertions` under the
data directory. A token is written there before the API token exchanged for it is returned, so a
restart forgets nothing and exchanges work as soon as the server is up.

- If the file is missing or does not read in full when the server starts (the first start with a
  federation rule, a lost data directory, a damaged file), the server cannot tell what was spent
  before. From then on it
  refuses every single-use identity token whose `iat` is earlier than 30 seconds after that start,
  the margin it allows between its own clock and the issuer's; the file remembers that line across
  later restarts. For about the first minute the refusal is `temporarily_unavailable`, because a
  fresh token gets through once the issuer's clock passes the line; retry with a fresh identity
  token. After that minute a token below the line is refused with `invalid_grant`.
- A record is dropped once its token has expired, and the file keeps the latest expiry it has
  dropped. A token expiring at or before that mark is refused, so a host clock that ran ahead and
  expired records early cannot reopen them when it is corrected. With a correct clock this changes
  nothing: such a token has expired anyway. After a clock error the refusal is
  `temporarily_unavailable`, and an identity token obtained later, which expires past the mark, is
  accepted. If a wrong clock left the mark or the line above far in the future, stop the server,
  remove the file and start it again; that start is treated like any other without the file.
- If a spent token cannot be written to the file, no API token is issued and the exchange answers
  `temporarily_unavailable`.
- The record is as current as the data directory. Restoring an earlier copy of the directory
  brings back an earlier record, and that is not detected.
- The record is kept per issuer URL, so renaming an issuer in the configuration keeps it.
- Servers do not share the record. Give each server its own `expected_audience`, so an identity
  token is accepted by one server only.

## Issued tokens

An issued token is an ordinary Hypeman API token signed with `jwt_secret`, with `sub` set to
`federation:<rule name>` and `permissions` set to the rule's list. It also names the rule it was
issued for. Once that rule or its issuer is removed or changed and the server restarted, the tokens
issued for it are rejected.

Servers that share a `jwt_secret` accept each other's issued tokens only when the rule and its
issuer are configured identically on both, `expected_audience` included. With an audience per
server, as recommended above, an issued token works only on the server that issued it.

`permissions` are enforced on the API routes listed in the [scopes](../lib/scopes/README.md)
documentation. Direct access to the built-in registry under `/v2` accepts any valid API token and
is not limited by `permissions`.

## Observability

- `hypeman_oidc_exchanges_total{issuer, rule, result}` counts exchanges. `result` is `issued`,
  `rejected` or `unavailable`. `issuer` and `rule` are empty when the caller named no configured
  rule.
- Each exchange logs one `oidc assertion exchange` line with the rule, the issuer, the result and
  the reason, plus the SHA-256 of the identity token's subject once its signature is verified and
  the `jti` of the issued token. Identity tokens and their other claims are never logged.
