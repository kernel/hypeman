package jwtauth

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/kernel/hypeman/lib/accesstoken"
	"github.com/kernel/hypeman/lib/accesstoken/accesstokentest"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/metric/noop"
)

func hmacToken(t *testing.T, secret string, header map[string]any) string {
	t.Helper()
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{"sub": "admin", "exp": time.Now().Add(time.Hour).Unix()})
	for name, value := range header {
		token.Header[name] = value
	}
	raw, err := token.SignedString([]byte(secret))
	require.NoError(t, err)
	return raw
}

func TestAccessTokensAreVerifiedAgainstTheAuthorizationServerOnly(t *testing.T) {
	server := accesstokentest.NewServer(t)
	hmacOnly, err := NewVerifier("secret", nil, noop.NewMeterProvider().Meter("test"))
	require.NoError(t, err)
	verifier := hmacOnly.WithAccessTokens(server.Verifier(t))

	accessClaims := accesstokentest.Claims("federation:ci", "instance:read image:read")
	accessClaims["jti"] = "token-1"
	access := server.Token(t, accessClaims, nil)

	// The claims arrive in the shape of an HMAC token's.
	claims := jwt.MapClaims{}
	token, err := verifier.ParseWithClaims(context.Background(), access, claims)
	require.NoError(t, err)
	require.True(t, token.Valid)
	require.Equal(t, "federation:ci", claims["sub"])
	require.Equal(t, []any{"instance:read", "image:read"}, claims["permissions"])
	require.Equal(t, "token-1", claims["jti"])
	require.NotContains(t, claims, "scope", "a scope claim marks a registry token to the callers")

	var registered jwt.RegisteredClaims
	token, err = verifier.ParseWithClaims(context.Background(), access, &registered)
	require.NoError(t, err)
	require.Same(t, &registered, token.Claims)
	require.Equal(t, "federation:ci", registered.Subject)
	require.Equal(t, jwt.ClaimStrings{accesstokentest.Audience}, registered.Audience)

	// A token without a scope still carries a permissions list, an empty one.
	unscoped := accesstokentest.Claims("user", "")
	delete(unscoped, "scope")
	claims = jwt.MapClaims{}
	_, err = verifier.ParseWithClaims(context.Background(), server.Token(t, unscoped, nil), claims)
	require.NoError(t, err)
	require.Equal(t, []any{}, claims["permissions"])

	// Tokens signed with the server's own secret work as before.
	claims = jwt.MapClaims{}
	_, err = verifier.ParseWithClaims(context.Background(), hmacToken(t, "secret", nil), claims)
	require.NoError(t, err)
	require.Equal(t, "admin", claims["sub"])
	require.NotContains(t, claims, "permissions")

	// Neither kind of key accepts the other kind of token.
	_, err = verifier.ParseWithClaims(context.Background(), hmacToken(t, "secret", map[string]any{"typ": "at+jwt"}), jwt.MapClaims{})
	require.ErrorIs(t, err, accesstoken.ErrToken, "the secret does not sign access tokens")
	_, err = verifier.ParseWithClaims(context.Background(), hmacToken(t, "secret", map[string]any{"typ": "at+jwt", "pad": strings.Repeat("a", 20<<10)}), jwt.MapClaims{})
	require.ErrorIs(t, err, accesstoken.ErrToken, "nor a large one")
	_, err = verifier.ParseWithClaims(context.Background(), server.Token(t, accessClaims, map[string]any{"typ": "JWT"}), jwt.MapClaims{})
	require.Error(t, err, "an authorization server key does not sign other tokens")
	require.NotErrorIs(t, err, accesstoken.ErrToken)

	// With no authorization server configured, an access token is just a
	// token the secret did not sign.
	_, err = hmacOnly.ParseWithClaims(context.Background(), access, jwt.MapClaims{})
	require.Error(t, err)
	disabled, err := accesstoken.New(accesstoken.Config{}, nil, nil)
	require.NoError(t, err)
	_, err = hmacOnly.WithAccessTokens(disabled).ParseWithClaims(context.Background(), access, jwt.MapClaims{})
	require.Error(t, err)
	require.NotErrorIs(t, err, accesstoken.ErrToken)
}
