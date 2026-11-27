# authserver

The OAuth authorization server behind `hypeman-auth`. Operator documentation is in
[docs/authorization-server.md](../../docs/authorization-server.md).

`Server.Exchange` checks an identity token against the federation rule the caller names, with
`lib/oidcfederation`, records it in the `SpentStore` if its issuer makes identity tokens
single-use, and signs an access token in the format `lib/accesstoken` verifies. `Server.Handler`
serves that exchange as the RFC 7523 token endpoint, beside the RFC 8414 metadata and the key set.

`PostgresStore` is the `SpentStore` for more than one instance: the insert on the primary key
decides which of two concurrent exchanges of one identity token is first. Its test needs a
database and is skipped unless `HYPEMAN_AUTH_TEST_POSTGRES_URL` is set.
