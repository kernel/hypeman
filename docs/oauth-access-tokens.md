# OAuth access tokens

Hypeman can accept access tokens issued by an OAuth authorization server, beside the tokens signed
with `jwt_secret`. For these tokens the server is a resource server: it holds the authorization
server's public keys, so it can verify the tokens and cannot issue them.

## Configure

```yaml
oauth:
  audiences:
    - https://hypeman.example.com
  authorization_servers:
    - issuer: https://auth.example.com
```

| Key | Meaning |
|---|---|
| `audiences` | The names this server answers to. A token is accepted only if its `aud` holds at least one of them. Servers that share an audience accept the same tokens. |
| `authorization_servers[].issuer` | The authorization server's issuer URL. It must equal a token's `iss` exactly. |
| `authorization_servers[].jwks` | Where the server's public keys come from; see [Keys](#keys). Optional. |

Restart after a change. A block that is not valid stops the server from starting, and so does a
key it does not know. `jwt_secret` is still required, and tokens signed with it work as before.

## What a token needs

Access tokens follow the JWT format of [RFC 9068](https://www.rfc-editor.org/rfc/rfc9068).

| Part | Requirement |
|---|---|
| `typ` header | `at+jwt`. A token without it is never checked against an authorization server's keys, and a token with it is never checked against `jwt_secret`. |
| `alg` header | An asymmetric algorithm: RS256, RS384, RS512, PS256, PS384, PS512, ES256, ES384, ES512 or EdDSA. The key must be of that algorithm's kind, and for ES256, ES384 and ES512 on its curve. |
| `kid` header | Names the signing key. It may be left out only when the server publishes a single key. |
| `iss` | A configured `issuer`. |
| `aud` | A string or a list holding at least one configured audience. |
| `sub` | Who the token is for. It is the user in logs. |
| `iat`, `exp` | Required. `nbf` is honoured if present. Clocks may differ by 30 seconds. |
| `scope` | Optional. Space-separated permissions such as `instance:read image:read`, or `*` for all of them. |
| `jti` | Optional. |

A token is at most 16 KB. Its lifetime is the authorization server's to choose.

A token without a `scope` has no permissions. This differs from a token signed with `jwt_secret`,
where a missing `permissions` claim means full access. As for every API token, the registry under
`/v2` does not check permissions.

`revoked_token_hashes` applies to access tokens as it does to the others. Revoking a token signed
with ES256, ES384 or ES512 also revokes the second token string that carries the same signature.

## Keys

`jwks.type` selects the source:

| Type | Source |
|---|---|
| `discovery` (default) | The key set named by `jwks_uri` in the server's [RFC 8414](https://www.rfc-editor.org/rfc/rfc8414) metadata, at `/.well-known/oauth-authorization-server`. |
| `explicit_url` | The key set at `jwks.url`. |
| `inline` | The public JWKs listed in `jwks.keys`. Nothing is fetched. |

`jwks.ca_cert_pem` replaces the system roots for the fetch.

Fetched keys are handled so that requests do not wait on the authorization server:

- Keys are fetched over HTTPS from public addresses only, without redirects or a proxy, and kept
  for five minutes. After that they are refreshed in the background while requests are still
  verified with the last key set.
- A token signed with a key the last set lacks triggers a refresh, at most one a minute. This is
  how a new signing key is picked up without a restart.
- The last key set is also saved under `<data_dir>/oauth/keys/`, with the time it was fetched. A
  server that restarts continues from that copy as if it had kept running: a copy fetched less
  than five minutes ago is used until those five minutes are over, and an older one is used while
  the key set is fetched again. Only a server with no saved copy, such as one that has just been
  configured, makes its first requests wait for a fetch.

A key removed from the authorization server stops being trusted at the next successful fetch, at
most five minutes later while the authorization server is reachable, whether or not the server
restarted in between. A key set that is empty, or holds only valid public keys for other purposes
such as encryption, is such a fetch too, and withdraws every key. While the authorization server
is unreachable or answers with anything else, the last key set stays trusted, for as long as that
lasts. To drop it at once, delete the saved copy and restart: until a fetch succeeds, no access
token is then accepted.

## Monitor

`hypeman_jwt_validations_total{key_role="authorization_server"}` counts verified access tokens.

A key fetch that fails is logged as a warning, with whether the last key set is still in use, at
most every ten seconds for each authorization server. A copy that could not be saved is logged as a
warning as well, and a start from the saved copy at info level, with the copy's fetch time.
