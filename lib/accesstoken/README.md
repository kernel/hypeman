# accesstoken

Verifies the access tokens an OAuth authorization server issues for this server, in the JWT format
of RFC 9068. Operator documentation is in
[docs/oauth-access-tokens.md](../../docs/oauth-access-tokens.md).

`New` validates the `oauth` block of the server configuration. `Verifier.Verify` selects the
authorization server a token names in `iss`, has `lib/oidcfederation` check the signature, type
and time claims against that server's keys, and then requires a subject, a client ID, a token ID
and one of this server's audiences. The token's `scope` becomes `Claims.Permissions`, which is never nil.

`lib/jwtauth` calls it for tokens whose `typ` header is `at+jwt` (`Typed`), and only for those.

`FileKeyStore` keeps each authorization server's last fetched key set in a file, with the time it
was fetched, so that a restarted server continues with the key cache it had.
