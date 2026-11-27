package accesstoken_test

import (
	"context"
	"encoding/base64"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/kernel/hypeman/lib/accesstoken"
	"github.com/kernel/hypeman/lib/accesstoken/accesstokentest"
	"github.com/kernel/hypeman/lib/oidcfederation"
	"github.com/stretchr/testify/require"
)

func TestVerifyAcceptsATokenIssuedForThisServer(t *testing.T) {
	server := accesstokentest.NewServer(t)
	verifier := server.Verifier(t)
	require.True(t, verifier.Enabled())

	claims := accesstokentest.Claims("federation:ci", "instance:read instance:write")
	claims["jti"] = "token-1"
	// A token may name other audiences beside this server's.
	claims["aud"] = []string{"https://other.example.test", accesstokentest.Audience}
	verified, err := verifier.Verify(context.Background(), server.Token(t, claims, nil))
	require.NoError(t, err)
	require.Equal(t, accesstokentest.Issuer, verified.Issuer)
	require.Equal(t, "federation:ci", verified.Subject)
	require.Equal(t, "token-1", verified.ID)
	require.Equal(t, "test-client", verified.ClientID)
	require.Equal(t, []string{"instance:read", "instance:write"}, verified.Permissions)
	require.Equal(t, claims["exp"], verified.ExpiresAt.Unix())
	require.Equal(t, claims["iat"], verified.IssuedAt.Unix())

	_, err = verifier.Verify(context.Background(), server.Token(t, accesstokentest.Claims("admin", "*"), map[string]any{"typ": "application/at+jwt"}))
	require.NoError(t, err)
}

// A token without a scope has no permissions, and the list is never nil: a
// nil list reads as the full access of a token that predates permissions.
func TestVerifyGivesATokenWithoutScopeNoPermissions(t *testing.T) {
	server := accesstokentest.NewServer(t)
	for _, scope := range []any{nil, "", "  "} {
		claims := accesstokentest.Claims("user", "")
		delete(claims, "scope")
		if scope != nil {
			claims["scope"] = scope
		}
		verified, err := server.Verifier(t).Verify(context.Background(), server.Token(t, claims, nil))
		require.NoError(t, err)
		require.NotNil(t, verified.Permissions)
		require.Empty(t, verified.Permissions)
	}
}

func TestVerifyRejectsOtherTokens(t *testing.T) {
	server := accesstokentest.NewServer(t)
	other := accesstokentest.NewServer(t)
	verifier := server.Verifier(t)
	now := time.Now()
	for name, alter := range map[string]func(jwt.MapClaims){
		"unknown issuer":    func(c jwt.MapClaims) { c["iss"] = "https://other.example.test" },
		"no issuer":         func(c jwt.MapClaims) { delete(c, "iss") },
		"other audience":    func(c jwt.MapClaims) { c["aud"] = "https://other.example.test" },
		"no audience":       func(c jwt.MapClaims) { delete(c, "aud") },
		"no subject":        func(c jwt.MapClaims) { delete(c, "sub") },
		"empty subject":     func(c jwt.MapClaims) { c["sub"] = "" },
		"no client":         func(c jwt.MapClaims) { delete(c, "client_id") },
		"empty client":      func(c jwt.MapClaims) { c["client_id"] = "" },
		"client not text":   func(c jwt.MapClaims) { c["client_id"] = 7 },
		"no token ID":       func(c jwt.MapClaims) { delete(c, "jti") },
		"empty token ID":    func(c jwt.MapClaims) { c["jti"] = "" },
		"no issued-at":      func(c jwt.MapClaims) { delete(c, "iat") },
		"no expiry":         func(c jwt.MapClaims) { delete(c, "exp") },
		"expired":           func(c jwt.MapClaims) { c["exp"] = now.Add(-time.Minute).Unix() },
		"issued later":      func(c jwt.MapClaims) { c["iat"] = now.Add(5 * time.Minute).Unix() },
		"not valid yet":     func(c jwt.MapClaims) { c["nbf"] = now.Add(5 * time.Minute).Unix() },
		"scope not text":    func(c jwt.MapClaims) { c["scope"] = []string{"instance:read"} },
		"token ID not text": func(c jwt.MapClaims) { c["jti"] = 12 },
	} {
		t.Run(name, func(t *testing.T) {
			claims := accesstokentest.Claims("user", "instance:read")
			alter(claims)
			_, err := verifier.Verify(context.Background(), server.Token(t, claims, nil))
			require.ErrorIs(t, err, accesstoken.ErrToken)
		})
	}

	claims := accesstokentest.Claims("user", "instance:read")
	for name, token := range map[string]string{
		// An identity token or any other JWT from the same issuer is not an access token.
		"typ JWT":            server.Token(t, claims, map[string]any{"typ": "JWT"}),
		"other typ":          server.Token(t, claims, map[string]any{"typ": "dpop+jwt"}),
		"another server key": other.Token(t, claims, nil),
		"unknown key ID":     server.Token(t, claims, map[string]any{"kid": "key-b"}),
		"not a token":        "at+jwt",
		"too large":          strings.Repeat("a", oidcfederation.MaxAssertionBytes) + ".e30.e30",
	} {
		t.Run(name, func(t *testing.T) {
			_, err := verifier.Verify(context.Background(), token)
			require.ErrorIs(t, err, accesstoken.ErrToken)
		})
	}

	// A token signed with the server's public key as an HMAC secret, or not
	// signed at all, declares an algorithm no authorization server key accepts.
	for _, method := range []jwt.SigningMethod{jwt.SigningMethodHS256, jwt.SigningMethodNone} {
		token := jwt.NewWithClaims(method, claims)
		token.Header["kid"], token.Header["typ"] = "key-a", "at+jwt"
		var key any = jwt.UnsafeAllowNoneSignatureType
		if method == jwt.SigningMethodHS256 {
			key = server.Key.PublicKey.X.Bytes()
		}
		raw, err := token.SignedString(key)
		require.NoError(t, err)
		_, err = verifier.Verify(context.Background(), raw)
		require.ErrorIs(t, err, accesstoken.ErrToken, method.Alg())
	}
}

func TestTypedReadsOnlyTheHeader(t *testing.T) {
	server := accesstokentest.NewServer(t)
	claims := accesstokentest.Claims("user", "")
	require.True(t, accesstoken.Typed(server.Token(t, claims, nil)))
	require.True(t, accesstoken.Typed(server.Token(t, claims, map[string]any{"typ": "AT+JWT"})))
	require.True(t, accesstoken.Typed(server.Token(t, claims, map[string]any{"typ": "application/at+jwt"})))
	require.False(t, accesstoken.Typed(server.Token(t, claims, map[string]any{"typ": "JWT"})))
	require.False(t, accesstoken.Typed(server.Token(t, claims, map[string]any{"typ": 1})))
	require.False(t, accesstoken.Typed("not.a.token"))
	require.False(t, accesstoken.Typed(""))

	// The member named exactly "typ" decides, as it does in verification.
	encode := base64.RawURLEncoding.EncodeToString
	require.False(t, accesstoken.Typed(encode([]byte(`{"alg":"ES256","typ":"JWT","TYP":"at+jwt"}`))+".e30.c2ln"))
	require.True(t, accesstoken.Typed(encode([]byte(`{"alg":"ES256","typ":"at+jwt","Typ":"JWT"}`))+".e30.c2ln"))

	// The type is read from the header whatever the size of the rest, so no
	// token escapes being treated as the access token it says it is.
	oversized := accesstokentest.Claims("user", strings.Repeat("instance:read ", 2000))
	token := server.Token(t, oversized, nil)
	require.Greater(t, len(token), oidcfederation.MaxAssertionBytes)
	require.True(t, accesstoken.Typed(token))
	_, err := server.Verifier(t).Verify(context.Background(), token)
	require.ErrorIs(t, err, accesstoken.ErrToken)
}

func TestNewValidatesTheConfiguration(t *testing.T) {
	disabled, err := accesstoken.New(accesstoken.Config{}, nil, nil)
	require.NoError(t, err)
	require.False(t, disabled.Enabled())
	_, err = disabled.Verify(context.Background(), "a.b.c")
	require.ErrorIs(t, err, accesstoken.ErrToken)
	var none *accesstoken.Verifier
	require.False(t, none.Enabled())

	valid := func() accesstoken.Config {
		return accesstoken.Config{Audiences: []string{"prod"}, AuthorizationServers: []accesstoken.AuthorizationServer{{Issuer: "https://auth.example.test"}}}
	}
	_, err = accesstoken.New(valid(), nil, nil)
	require.NoError(t, err)
	for name, alter := range map[string]func(*accesstoken.Config){
		"audiences without a server": func(c *accesstoken.Config) { c.AuthorizationServers = nil },
		"no audience":                func(c *accesstoken.Config) { c.Audiences = nil },
		"empty audience":             func(c *accesstoken.Config) { c.Audiences = []string{""} },
		"repeated audience":          func(c *accesstoken.Config) { c.Audiences = []string{"prod", "prod"} },
		"plain http issuer":          func(c *accesstoken.Config) { c.AuthorizationServers[0].Issuer = "http://auth.example.test" },
		"no issuer":                  func(c *accesstoken.Config) { c.AuthorizationServers[0].Issuer = "" },
		"issuer twice": func(c *accesstoken.Config) {
			c.AuthorizationServers = append(c.AuthorizationServers, c.AuthorizationServers[0])
		},
		"unknown key source":  func(c *accesstoken.Config) { c.AuthorizationServers[0].JWKS.Type = "file" },
		"inline without keys": func(c *accesstoken.Config) { c.AuthorizationServers[0].JWKS.Type = "inline" },
		"metadata from elsewhere": func(c *accesstoken.Config) {
			c.AuthorizationServers[0].JWKS.DiscoveryBaseURL = "https://metadata.example.test"
		},
	} {
		t.Run(name, func(t *testing.T) {
			cfg := valid()
			alter(&cfg)
			_, err := accesstoken.New(cfg, nil, nil)
			require.ErrorIs(t, err, accesstoken.ErrInvalidConfig)
		})
	}
}

func TestFileKeyStoreKeepsOneCopyPerID(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "oauth", "keys")
	store := accesstoken.NewFileKeyStore(dir, slog.New(slog.DiscardHandler))
	first, second := strings.Repeat("a", 64), strings.Repeat("b", 64)
	_, found := store.Load(first)
	require.False(t, found)

	store.Save(first, "https://auth.example.test", []byte(`{"keys":[1]}`))
	store.Save(second, "https://other.example.test", []byte(`{"keys":[2]}`))
	store.Save(first, "https://auth.example.test", []byte(`{"keys":[3]}`))
	keySet, found := store.Load(first)
	require.True(t, found)
	require.JSONEq(t, `{"keys":[3]}`, string(keySet))
	keySet, found = store.Load(second)
	require.True(t, found)
	require.JSONEq(t, `{"keys":[2]}`, string(keySet))

	// Only the verifier's hex digests name a file.
	for _, id := range []string{"", "../" + first, strings.Repeat("A", 64), first + "0"} {
		store.Save(id, "https://auth.example.test", []byte(`{"keys":[4]}`))
		_, found = store.Load(id)
		require.False(t, found, id)
	}

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	require.Len(t, entries, 2, "no temporary or stray file is left behind")
	for _, entry := range entries {
		info, err := entry.Info()
		require.NoError(t, err)
		require.Equal(t, os.FileMode(0o600), info.Mode().Perm())
		require.NoError(t, os.WriteFile(filepath.Join(dir, entry.Name()), make([]byte, 300<<10), 0o600))
	}
	_, found = store.Load(first)
	require.False(t, found, "a copy larger than a key set can be is not read")
}
