# oidcfederation

Validates OIDC identity tokens from operator-configured issuers, so a workload can prove who it
is with a token it already holds instead of a stored Hypeman API token.

Trust is two kinds of object, both written in the `oidc` block of the server configuration:

- An **issuer** names who signs: an issuer URL, where its public keys come from (`discovery`,
  `explicit_url` or `inline`), the longest assertion lifetime accepted, and whether an assertion
  may be presented more than once.
- A **federation rule** names which workload of one issuer is accepted and what it receives: the
  audience the assertion must carry, the conditions its claims must meet, a `permissions` scope
  list and a token lifetime.

`Compile` validates a configuration and returns a `Registry` of rules. `Verifier.Verify` checks one
assertion against a rule's issuer and audience, and `Match.Matches` evaluates the rule's conditions
on the verified claims.

## Presets

Each issuer has a `preset_type`, and a rule's `match.type` must be the same type. A preset carries
its provider's mandatory identity checks, so a rule cannot trade them for a generic matcher.

| Preset | Recognized by | A rule must name |
|---|---|---|
| `github_actions` | issuer URL | numeric `repository_id` and `repository_owner_id`, one of `workflow`, `workflow_ref` or `job_workflow_ref`, and `ref` or `environment`. `event_names` defaults to `push` and `workflow_dispatch`. |
| `google_cloud` | issuer URL | a numeric `subject` |
| `microsoft_entra` | issuer URL | `subject`, or an `/oid`, `/appid` or `/azp` claim. The tenant is pinned from the issuer URL. Tokens carrying delegated scopes are refused. |
| `kubernetes` | configuration | `namespace` and `service_account` |
| `aws`, `custom_oidc` | configuration | `subject`, `subject_prefix` or at least one claim |

`claims` maps a JSON Pointer to the text a scalar claim must equal. Booleans and numbers compare by
their JSON text; objects and arrays never match.

## Verification

- The token's `iss` must equal the configured issuer URL before any key is fetched. Key locations
  come only from configuration, never from the token.
- Keys are fetched over HTTPS from public addresses only, without redirects or an ambient proxy,
  and cached for five minutes. An unknown `kid` forces a refresh: after one that succeeds, the
  next waits a minute; after a fetch that fails, the next attempt waits ten seconds. Keys
  published for another purpose, such as encryption, are ignored.
- Only asymmetric signature algorithms are accepted, and only RS256 for GitHub, Google and Entra.
- An assertion needs `sub`, `iat`, `exp` and exactly one `aud` equal to the rule's
  `expected_audience`. Its lifetime must fit the issuer's `max_lifetime_seconds`.
- A single-use issuer's assertion needs a token ID (`jti`, or `uti` for Entra). GitHub and AWS are
  always single-use. Google identity tokens carry no ID, so Google always allows reuse. Kubernetes
  allows reuse by default, because a pod reads the same projected token until it is rotated.

The verifier reports only that an assertion is invalid or that keys are unavailable. A
`KeyProblem` carries the reason an issuer's keys cannot be used, for the operator's logs.
