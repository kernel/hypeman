package authserver

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/golang-jwt/jwt/v5"
	"github.com/kernel/hypeman/lib/accesstoken"
	"github.com/kernel/hypeman/lib/oidcfederation"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/metric/noop"
)

const (
	testIssuer   = "https://auth.example.test"
	testIdP      = "https://idp.example.test"
	testAudience = "https://hypeman.example.test"
)

// memorySpent is a SpentStore for one process.
type memorySpent struct {
	mu    sync.Mutex
	spent map[string]time.Time
	err   error
}

func (m *memorySpent) Spend(_ context.Context, id string, expires time.Time) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return false, m.err
	}
	if _, exists := m.spent[id]; exists {
		return false, nil
	}
	if m.spent == nil {
		m.spent = map[string]time.Time{}
	}
	m.spent[id] = expires
	return true, nil
}

// fixture is an identity provider whose public key is configured inline, a
// rule that trusts one of its workloads, and a server with that rule.
type fixture struct {
	idp    *ecdsa.PrivateKey
	config oidcfederation.Config
	spent  *memorySpent
	server *Server
}

func newFixture(t *testing.T, alter func(*oidcfederation.Config)) *fixture {
	t.Helper()
	idp, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	raw, err := json.Marshal(jose.JSONWebKey{Key: &idp.PublicKey, KeyID: "idp-key", Algorithm: "ES256", Use: "sig"})
	require.NoError(t, err)
	var public map[string]any
	require.NoError(t, json.Unmarshal(raw, &public))
	f := &fixture{idp: idp, spent: &memorySpent{}}
	f.config = oidcfederation.Config{
		Issuers: []oidcfederation.IssuerSpec{{Name: "idp", IssuerURL: testIdP, JWKS: oidcfederation.KeySource{Type: "inline", Keys: []map[string]any{public}}}},
		FederationRules: []oidcfederation.RuleSpec{{
			Name: "ci", Issuer: "idp", ExpectedAudience: testIssuer,
			Match:         oidcfederation.Match{Type: "custom_oidc", Subject: "build"},
			Authorization: oidcfederation.RuleAuthorization{Audiences: []string{testAudience}, Permissions: []string{"instance:write", "instance:read"}},
		}},
	}
	if alter != nil {
		alter(&f.config)
	}
	f.server = f.start(t, f.spent)
	return f
}

// start returns another instance of the server on the same configuration.
func (f *fixture) start(t *testing.T, spent SpentStore) *Server {
	t.Helper()
	rules, err := oidcfederation.Compile(f.config)
	require.NoError(t, err)
	server, err := New(Options{Issuer: testIssuer, Key: newSigningKey(t), Rules: rules, Spent: spent, Meter: noop.NewMeterProvider().Meter("test"), Log: discardLog})
	require.NoError(t, err)
	return server
}

// identity signs an identity token the fixture's rule accepts. It outlives
// any access token issued for it, so that lifetimes are the rule's.
func (f *fixture) identity(t *testing.T, alter func(jwt.MapClaims)) string {
	t.Helper()
	var id [16]byte
	_, _ = rand.Read(id[:])
	now := time.Now()
	claims := jwt.MapClaims{"iss": testIdP, "sub": "build", "aud": testIssuer, "iat": now.Unix(), "exp": now.Add(30 * time.Minute).Unix(), "jti": hex.EncodeToString(id[:])}
	if alter != nil {
		alter(claims)
	}
	token := jwt.NewWithClaims(jwt.SigningMethodES256, claims)
	token.Header["kid"] = "idp-key"
	raw, err := token.SignedString(f.idp)
	require.NoError(t, err)
	return raw
}

// resource returns what a Hypeman server configured to trust the fixture's
// server for audience makes of a token: its own verifier, with the server's
// published key set.
func (f *fixture) resource(t *testing.T, server *Server, audience string) *accesstoken.Verifier {
	t.Helper()
	var set struct {
		Keys []map[string]any `json:"keys"`
	}
	require.NoError(t, json.Unmarshal(server.keySet, &set))
	cfg := accesstoken.Config{Audiences: []string{audience}, AuthorizationServers: []accesstoken.AuthorizationServer{{Issuer: testIssuer}}}
	cfg.AuthorizationServers[0].JWKS.Type = "inline"
	cfg.AuthorizationServers[0].JWKS.Keys = set.Keys
	verifier, err := accesstoken.New(cfg, nil, nil)
	require.NoError(t, err)
	return verifier
}

func TestExchangeIssuesATokenAHypemanServerAccepts(t *testing.T) {
	f := newFixture(t, nil)
	token, err := f.server.Exchange(context.Background(), "ci", f.identity(t, nil), "")
	require.NoError(t, err)
	require.Equal(t, oidcfederation.DefaultTokenTTL, token.ExpiresIn)
	require.Equal(t, "instance:read instance:write", token.Scope)

	verified, err := f.resource(t, f.server, testAudience).Verify(context.Background(), token.AccessToken)
	require.NoError(t, err)
	require.Equal(t, testIssuer, verified.Issuer)
	require.Equal(t, "federation:ci", verified.Subject)
	require.Equal(t, "ci", verified.ClientID)
	require.Len(t, verified.ID, 32)
	require.Equal(t, []string{testAudience}, verified.Audience)
	require.Equal(t, []string{"instance:read", "instance:write"}, verified.Permissions)
	require.Equal(t, int64(oidcfederation.DefaultTokenTTL), verified.ExpiresAt.Unix()-verified.IssuedAt.Unix())

	// A server with another audience, or one that trusts another key, refuses it.
	_, err = f.resource(t, f.server, "https://other.example.test").Verify(context.Background(), token.AccessToken)
	require.ErrorIs(t, err, accesstoken.ErrToken)
	_, err = f.resource(t, f.start(t, f.spent), testAudience).Verify(context.Background(), token.AccessToken)
	require.ErrorIs(t, err, accesstoken.ErrToken)

	parsed, _, err := jwt.NewParser().ParseUnverified(token.AccessToken, jwt.MapClaims{})
	require.NoError(t, err)
	require.Equal(t, "at+jwt", parsed.Header["typ"])
	require.Equal(t, "ES256", parsed.Header["alg"])
	require.Equal(t, f.server.key.ID, parsed.Header["kid"])
}

// An identity token from a single-use issuer buys one access token, from
// whichever instance of the server it reaches.
func TestIdentityTokenIsSpentAcrossInstances(t *testing.T) {
	f := newFixture(t, nil)
	other := f.start(t, f.spent)
	identity := f.identity(t, nil)
	_, err := f.server.Exchange(context.Background(), "ci", identity, "")
	require.NoError(t, err)
	for _, server := range []*Server{f.server, other} {
		_, err = server.Exchange(context.Background(), "ci", identity, "")
		require.ErrorIs(t, err, ErrGrant)
	}
	_, err = other.Exchange(context.Background(), "ci", f.identity(t, nil), "")
	require.NoError(t, err)

	// Raced, exactly one exchange of a token succeeds.
	raced := f.identity(t, nil)
	results := make(chan error, 16)
	for i := range cap(results) {
		go func() {
			_, err := []*Server{f.server, other}[i%2].Exchange(context.Background(), "ci", raced, "")
			results <- err
		}()
	}
	issued := 0
	for range cap(results) {
		if err := <-results; err == nil {
			issued++
		} else {
			require.ErrorIs(t, err, ErrGrant)
		}
	}
	require.Equal(t, 1, issued)
}

// Nothing is issued for an identity token that could not be recorded, and the
// identity token is still good once the store is back.
func TestNoTokenWithoutARecordOfTheSpentIdentityToken(t *testing.T) {
	f := newFixture(t, nil)
	identity := f.identity(t, nil)
	f.spent.err = errors.New("connection refused")
	_, err := f.server.Exchange(context.Background(), "ci", identity, "")
	require.ErrorIs(t, err, ErrUnavailable)
	f.spent.err = nil
	_, err = f.server.Exchange(context.Background(), "ci", identity, "")
	require.NoError(t, err)

	// A single-use issuer cannot be configured without a store at all.
	rules, err := oidcfederation.Compile(f.config)
	require.NoError(t, err)
	_, err = New(Options{Issuer: testIssuer, Key: newSigningKey(t), Rules: rules, Meter: noop.NewMeterProvider().Meter("test"), Log: discardLog})
	require.ErrorContains(t, err, "replay store")
}

// An issuer whose identity tokens may be presented again needs no store.
func TestReusableIdentityTokensNeedNoStore(t *testing.T) {
	f := newFixture(t, func(c *oidcfederation.Config) {
		c.Issuers[0].TokenValidation.ReplayProtection = oidcfederation.ReplayAllowReuse
	})
	server := f.start(t, nil)
	identity := f.identity(t, func(c jwt.MapClaims) { delete(c, "jti") })
	for range 2 {
		_, err := server.Exchange(context.Background(), "ci", identity, "")
		require.NoError(t, err)
	}
}

func TestExchangeRefusals(t *testing.T) {
	f := newFixture(t, nil)
	for name, tc := range map[string]struct {
		rule  string
		alter func(jwt.MapClaims)
	}{
		"unknown rule":        {"other", nil},
		"another workload":    {"ci", func(c jwt.MapClaims) { c["sub"] = "deploy" }},
		"another audience":    {"ci", func(c jwt.MapClaims) { c["aud"] = testAudience }},
		"another issuer":      {"ci", func(c jwt.MapClaims) { c["iss"] = "https://other.example.test" }},
		"expired":             {"ci", func(c jwt.MapClaims) { c["exp"] = time.Now().Add(-time.Minute).Unix() }},
		"no token ID":         {"ci", func(c jwt.MapClaims) { delete(c, "jti") }},
		"lifetime too long":   {"ci", func(c jwt.MapClaims) { c["exp"] = time.Now().Add(3 * time.Hour).Unix() }},
		"issue time unusable": {"ci", func(c jwt.MapClaims) { c["iat"] = "now" }},
		"not valid yet":       {"ci", func(c jwt.MapClaims) { c["nbf"] = time.Now().Add(time.Hour).Unix() }},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := f.server.Exchange(context.Background(), tc.rule, f.identity(t, tc.alter), "")
			require.ErrorIs(t, err, ErrGrant)
		})
	}
	_, err := f.server.Exchange(context.Background(), "ci", "not a token", "")
	require.ErrorIs(t, err, ErrGrant)
	require.Empty(t, f.spent.spent, "a refused identity token is not spent")
}

// A rule may allow several audiences. The caller names the one it wants, and
// is told when it may not have it only after it has proven who it is.
func TestExchangeIssuesForOneOfTheRulesAudiences(t *testing.T) {
	f := newFixture(t, func(c *oidcfederation.Config) {
		c.FederationRules[0].Authorization.Audiences = []string{"prod", "staging"}
	})
	token, err := f.server.Exchange(context.Background(), "ci", f.identity(t, nil), "staging")
	require.NoError(t, err)
	verified, err := f.resource(t, f.server, "staging").Verify(context.Background(), token.AccessToken)
	require.NoError(t, err)
	require.Equal(t, []string{"staging"}, verified.Audience)
	_, err = f.resource(t, f.server, "prod").Verify(context.Background(), token.AccessToken)
	require.ErrorIs(t, err, accesstoken.ErrToken)

	identity := f.identity(t, nil)
	for _, audience := range []string{"", "dev", "prod staging"} {
		_, err = f.server.Exchange(context.Background(), "ci", identity, audience)
		require.ErrorIs(t, err, ErrTarget, audience)
	}
	_, err = f.server.Exchange(context.Background(), "ci", identity, "prod")
	require.NoError(t, err, "asking for the wrong audience did not spend the identity token")

	_, err = f.server.Exchange(context.Background(), "ci", f.identity(t, func(c jwt.MapClaims) { c["sub"] = "deploy" }), "dev")
	require.ErrorIs(t, err, ErrGrant, "a caller that is not the rule's workload learns nothing about audiences")

	// With one audience the caller need not name it, and may not name another.
	single := newFixture(t, nil)
	_, err = single.server.Exchange(context.Background(), "ci", single.identity(t, nil), testAudience)
	require.NoError(t, err)
	_, err = single.server.Exchange(context.Background(), "ci", single.identity(t, nil), "prod")
	require.ErrorIs(t, err, ErrTarget)
}

func TestTokenLifetimeIsClippedToTheIdentityToken(t *testing.T) {
	ttl := 600
	f := newFixture(t, func(c *oidcfederation.Config) { c.FederationRules[0].Authorization.MaxTokenTTLSeconds = &ttl })
	token, err := f.server.Exchange(context.Background(), "ci", f.identity(t, nil), "")
	require.NoError(t, err)
	require.Equal(t, 600, token.ExpiresIn)

	token, err = f.server.Exchange(context.Background(), "ci", f.identity(t, func(c jwt.MapClaims) { c["exp"] = time.Now().Add(40 * time.Second).Unix() }), "")
	require.NoError(t, err)
	require.InDelta(t, 39, token.ExpiresIn, 1.5, "the identity token has forty seconds left")

	// An identity token that runs out while it is being recorded buys nothing.
	clock := time.Now()
	f.server.now = func() time.Time { clock = clock.Add(20 * time.Minute); return clock }
	_, err = f.server.Exchange(context.Background(), "ci", f.identity(t, nil), "")
	require.ErrorIs(t, err, ErrGrant)
}

// The subject names the rule, and the caller too when the rule says which
// claim of the identity token names it.
func TestSubjectNamesTheCallerWhenTheRuleSaysHow(t *testing.T) {
	f := newFixture(t, func(c *oidcfederation.Config) { c.FederationRules[0].Authorization.SubjectClaim = "/email" })
	token, err := f.server.Exchange(context.Background(), "ci", f.identity(t, func(c jwt.MapClaims) { c["email"] = "dev@example.test" }), "")
	require.NoError(t, err)
	verified, err := f.resource(t, f.server, testAudience).Verify(context.Background(), token.AccessToken)
	require.NoError(t, err)
	require.Equal(t, "federation:ci:dev@example.test", verified.Subject)

	for name, email := range map[string]any{"missing": nil, "empty": "", "an object": map[string]any{"a": 1}, "a control character": "dev\n@example.test", "too long": string(make([]byte, 300))} {
		_, err = f.server.Exchange(context.Background(), "ci", f.identity(t, func(c jwt.MapClaims) {
			if email != nil {
				c["email"] = email
			}
		}), "")
		require.ErrorIs(t, err, ErrGrant, name)
	}
}

func TestNewValidatesTheIssuer(t *testing.T) {
	for _, issuer := range []string{"", "auth.example.test", "http://auth.example.test", "https://auth.example.test/tenant", "https://auth.example.test?x=1", "https://auth.example.test?", "https://auth.example.test#", "https://auth.example.test/%2F", "https://:443", "https://user@auth.example.test"} {
		_, err := New(Options{Issuer: issuer, Key: newSigningKey(t), Meter: noop.NewMeterProvider().Meter("test"), Log: discardLog})
		require.Error(t, err, issuer)
	}
	server, err := New(Options{Issuer: testIssuer + "/", Key: newSigningKey(t), Meter: noop.NewMeterProvider().Meter("test"), Log: discardLog})
	require.NoError(t, err)
	require.Equal(t, testIssuer, server.issuer)
	_, err = New(Options{Issuer: testIssuer, Meter: noop.NewMeterProvider().Meter("test"), Log: discardLog})
	require.ErrorContains(t, err, "signing key")
}
