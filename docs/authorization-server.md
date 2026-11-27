# Authorization server

`hypeman-auth` is an OAuth authorization server for Hypeman. A workload proves who it is with an
OIDC identity token it already has, from GitHub Actions, AWS, Google Cloud, Microsoft Entra,
Kubernetes or another OIDC issuer, and receives a short-lived access token. Hypeman servers verify
that token with the authorization server's public keys; see
[OAuth access tokens](oauth-access-tokens.md). No Hypeman server holds a key that can issue one.

It is a separate program from `hypeman-api` and needs no hypervisor: `go build ./cmd/auth`, or
the image built by `cmd/auth/Dockerfile`. Several instances can run behind one address.

## Configure

Configuration comes from a YAML file named by `CONFIG_PATH`, from environment variables, or both;
the environment wins. A key that is not known is an error, and so is a number or `true` written
where text belongs: write IDs and other text values in quotes, because a bare number is not
converted.

```yaml
issuer: https://auth.example.com
signing_key:
  file: /etc/hypeman-auth/signing-key.pem
replay_store:
  postgres_url: postgres://hypeman_auth:...@db.example.com:5432/hypeman_auth?sslmode=verify-full
oidc:
  issuers:
    - name: github-actions
      preset_type: github_actions
  federation_rules:
    - name: ci-nightly
      issuer: github-actions
      expected_audience: https://auth.example.com
      match:
        type: github_actions
        repository_id: "123"
        repository_owner_id: "456"
        workflow: .github/workflows/nightly.yml
        ref: refs/heads/main
      authorization:
        audiences: [https://hypeman.example.com]
        permissions: [instance:read, instance:write]
```

| Key | Environment | Meaning |
|---|---|---|
| `issuer` | `ISSUER` | The server's URL, without a path. It is the `iss` of every token and the address Hypeman servers are configured with, so it should not change. |
| `listen_address` | `LISTEN_ADDRESS` | Default `:8080`. The server speaks plain HTTP and expects TLS to end in front of it. |
| `metrics_listen_address` | `METRICS_LISTEN_ADDRESS` | Serves Prometheus metrics at `/metrics` on a separate listener. Off by default. |
| `signing_key.file`, `signing_key.pem` | `SIGNING_KEY__FILE`, `SIGNING_KEY__PEM` | The ECDSA P-256 private key tokens are signed with, as a PEM file or as PEM text. |
| `published_keys` | `PUBLISHED_KEYS` (JSON) | Public keys published beside the signing key; see [Rotate the signing key](#rotate-the-signing-key). |
| `replay_store.postgres_url` | `REPLAY_STORE__POSTGRES_URL` | A PostgreSQL database shared by every instance; see [Single use](#single-use). |
| `oidc` | `OIDC` (JSON) | The trusted identity issuers and the federation rules. From the environment it replaces the file's block as a whole. |
| `otel_endpoint` | `OTEL_ENDPOINT` | An OTLP gRPC collector to push metrics to, as scheme, host and optional port, such as `http://collector:4317`; `http` means without TLS. Off by default. |

Create a signing key with `openssl genpkey -algorithm EC -pkeyopt ec_paramgen_curve:P-256`.

### Issuers and rules

An issuer says who signs identity tokens, and a rule says which workload of one issuer gets what.
Their checks are described in [`lib/oidcfederation`](../lib/oidcfederation/README.md). A rule's
`authorization` holds what this server adds:

| Key | Meaning |
|---|---|
| `audiences` | The audiences a token may be issued for, each a name some Hypeman server lists in its `oauth.audiences`. Required. |
| `permissions` | The token's scope. Required, and never `*`. |
| `max_token_ttl_seconds` | Default 300, at most 600. A token never outlives the identity token it was issued for. |
| `subject_claim` | Optional JSON Pointer to the identity token claim that names the caller, such as `/email`. The token's subject is then `federation:<rule>:<value>`; without it, `federation:<rule>`. |

`expected_audience` is the audience the workload asks its identity provider for. Use the
authorization server's own URL.

## Get a token

```sh
curl https://auth.example.com/oauth/token \
  --data-urlencode grant_type=urn:ietf:params:oauth:grant-type:jwt-bearer \
  --data-urlencode assertion="$IDENTITY_TOKEN" \
  --data-urlencode federation_rule_id=ci-nightly \
  --data-urlencode resource=https://hypeman.example.com
```

```json
{"access_token":"...","token_type":"Bearer","expires_in":300,"scope":"instance:read instance:write"}
```

The request is the JWT bearer grant of [RFC 7523](https://www.rfc-editor.org/rfc/rfc7523), with
the audience named as in [RFC 8707](https://www.rfc-editor.org/rfc/rfc8707). `resource` may be
left out when the rule has a single audience. The request carries no client authentication, the
parameters go in the body, and parameters other than these four are refused.

| Error | Status | Meaning |
|---|---|---|
| `invalid_request` | 400 | The request is malformed, has parameters in the URL, repeats a parameter, carries an unknown one or an `Authorization` header. This is checked before the grant type. |
| `unsupported_grant_type` | 400 | Another grant type, in a request that is otherwise well-formed. |
| `invalid_grant` | 400 | The identity token was not accepted. The answer is the same whatever the reason: an unknown rule, a signature, a claim, a token already used. The reason is in the server's log. |
| `invalid_target` | 400 | The caller is the rule's workload, and the rule does not allow the audience it asked for. |
| `temporarily_unavailable` | 503 | No token can be issued right now: the issuer's keys or the replay store cannot be reached. Retry. |

Hypeman servers find the keys through `/.well-known/oauth-authorization-server`, which names the
key set at `/.well-known/jwks.json`. `/healthz` answers 204.

## Single use

Identity tokens from GitHub Actions and AWS, and by default from Entra and custom issuers, are
accepted once. The server records each one in the table `public.hypeman_auth_spent_assertions` by
a single insert on the primary key: two instances given the same identity token cannot both issue
for it. The record holds a hash of the issuer and token ID and the expiry, nothing else, and is
deleted a day after the identity token expired.

Every instance must use the same database, as the same role. The server creates the table and
its index at every start unless they exist, and PostgreSQL checks the privileges for that either
way: the role needs `USAGE` and `CREATE` on the `public` schema and must own the table. For a
database on another machine use `sslmode=verify-full`: the server acts on the result of the
insert, and the connection carries a password.

If the insert fails no token is issued. The identity token is usually still good, but the
database may have recorded it before the failure showed, so a caller whose retry is answered
`invalid_grant` should get a new identity token. The server does not start without
`replay_store.postgres_url` when any rule's issuer is single-use. Issuers whose identity tokens
may be presented again (Google Cloud, and Kubernetes by default) need no store.

## Rotate the signing key

A token's `kid` is the thumbprint of the key that signed it, so keys need no names.

1. Add the new key's public half to `published_keys` and restart.
2. Make the new key the `signing_key`, move the old key's public half to `published_keys`, and
   restart.
3. Once every token the old key signed has expired, remove it from `published_keys` and restart.

A Hypeman server keeps a key set for five minutes and fetches it again when a request needs it
after that, so it learns of a published or removed key at its first fetch after those five
minutes, and of a key ID it does not know at once, or within a minute if it has just fetched.
Leave that long between the steps. The key set is served with `Cache-Control: no-cache`; do not
put a cache in front of it.

For a compromised key, do steps 2 and 3 at once: replace `signing_key` and leave the old key out
of `published_keys`. Each Hypeman server stops trusting the old key at its next successful fetch.
To cut it off on a server without waiting for that, delete the server's saved copy and restart
it, as described in [OAuth access tokens](oauth-access-tokens.md#keys).

## Monitor

- `hypeman_auth_exchanges_total{issuer,rule,result}` counts exchanges; `result` is `issued`,
  `rejected` or `unavailable`.
- Each exchange is one log line with the rule, the issuer, the result, the reason, a hash of the
  identity token's subject and, when a token was issued, its `jti` and audience. Identity tokens
  and access tokens are never logged.
