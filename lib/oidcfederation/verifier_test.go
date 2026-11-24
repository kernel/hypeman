package oidcfederation

import (
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/golang-jwt/jwt/v5"
	"github.com/stretchr/testify/require"
)

type assertionFixture struct {
	key  *rsa.PrivateKey
	now  time.Time
	keys []byte
}

func newAssertionFixture(t *testing.T) assertionFixture {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	raw, err := json.Marshal(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &key.PublicKey, KeyID: "key-a", Algorithm: "RS256", Use: "sig"}}})
	require.NoError(t, err)
	return assertionFixture{key, time.Unix(1700000000, 0), raw}
}

func (f assertionFixture) claims() jwt.MapClaims {
	var nonce [16]byte
	_, _ = rand.Read(nonce[:])
	return jwt.MapClaims{"iss": GitHubIssuer, "sub": "repo:example/tests:environment:staging", "aud": "https://auth.example.test", "iat": f.now.Unix(), "exp": f.now.Add(5 * time.Minute).Unix(), "jti": base64.RawURLEncoding.EncodeToString(nonce[:])}
}

func (f assertionFixture) sign(t *testing.T, claims jwt.MapClaims, header map[string]any) string {
	t.Helper()
	token := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	token.Header["kid"] = "key-a"
	for key, value := range header {
		token.Header[key] = value
	}
	raw, err := token.SignedString(f.key)
	require.NoError(t, err)
	return raw
}

func (f assertionFixture) trust(t *testing.T) IssuerSpec {
	t.Helper()
	return githubRule(t).IssuerSpec
}

func TestVerifySignedAssertionAndCacheKeys(t *testing.T) {
	f := newAssertionFixture(t)
	v := NewVerifier()
	v.now = func() time.Time { return f.now }
	var calls atomic.Int32
	v.fetch = func(context.Context, IssuerSpec) ([]byte, error) { calls.Add(1); return f.keys, nil }
	for range 2 {
		claims := f.claims()
		result, err := v.Verify(context.Background(), f.trust(t), "https://auth.example.test", f.sign(t, claims, nil))
		require.NoError(t, err)
		require.Equal(t, claims["sub"], result.Subject)
		require.Equal(t, claims["jti"], result.JTI)
		require.Equal(t, f.now, result.IssuedAt)
		require.Equal(t, f.now.Add(5*time.Minute), result.ExpiresAt)
		require.True(t, result.SingleUse)
	}
	require.EqualValues(t, 1, calls.Load())
}

func TestVerifyRejectsInvalidClaimsAndProfiles(t *testing.T) {
	f := newAssertionFixture(t)
	v := NewVerifier()
	v.now = func() time.Time { return f.now }
	v.fetch = func(context.Context, IssuerSpec) ([]byte, error) { return f.keys, nil }
	for name, alter := range map[string]func(jwt.MapClaims){
		"issuer":        func(c jwt.MapClaims) { c["iss"] = "https://other.example.test" },
		"audience":      func(c jwt.MapClaims) { c["aud"] = "https://other.example.test" },
		"multiple-aud":  func(c jwt.MapClaims) { c["aud"] = []string{"https://auth.example.test", "other"} },
		"no-subject":    func(c jwt.MapClaims) { delete(c, "sub") },
		"no-issued-at":  func(c jwt.MapClaims) { delete(c, "iat") },
		"no-expiry":     func(c jwt.MapClaims) { delete(c, "exp") },
		"expired":       func(c jwt.MapClaims) { c["exp"] = f.now.Add(-time.Second).Unix() },
		"future-iat":    func(c jwt.MapClaims) { c["iat"] = f.now.Add(time.Minute).Unix() },
		"future-nbf":    func(c jwt.MapClaims) { c["nbf"] = f.now.Add(time.Minute).Unix() },
		"long-lifetime": func(c jwt.MapClaims) { c["exp"] = f.now.Add(2 * time.Hour).Unix() },
		"no-jti":        func(c jwt.MapClaims) { delete(c, "jti") },
		"nonstring-jti": func(c jwt.MapClaims) { c["jti"] = 12 },
	} {
		t.Run(name, func(t *testing.T) {
			claims := f.claims()
			alter(claims)
			_, err := v.Verify(context.Background(), f.trust(t), "https://auth.example.test", f.sign(t, claims, nil))
			require.ErrorIs(t, err, ErrAssertion)
		})
	}
	for _, header := range []map[string]any{{"typ": "oauth-wag+jwt"}, {"typ": "oauth-id-jag+jwt"}, {"typ": "at+jwt"}, {"kid": 1}, {"crit": []string{"unknown"}}} {
		_, err := v.Verify(context.Background(), f.trust(t), "https://auth.example.test", f.sign(t, f.claims(), header))
		require.ErrorIs(t, err, ErrAssertion)
	}
}

// Google identity tokens carry no token ID, and Entra names its token ID uti.
func TestVerifyUsesEachProvidersTokenID(t *testing.T) {
	f := newAssertionFixture(t)
	v := NewVerifier()
	v.now = func() time.Time { return f.now }
	v.fetch = func(context.Context, IssuerSpec) ([]byte, error) { return f.keys, nil }
	verify := func(spec IssuerSpec, alter func(jwt.MapClaims)) (Assertion, error) {
		issuer, err := normalizeIssuer(spec)
		require.NoError(t, err)
		claims := f.claims()
		claims["iss"] = issuer.IssuerURL
		delete(claims, "jti")
		alter(claims)
		return v.Verify(context.Background(), issuer, "https://auth.example.test", f.sign(t, claims, nil))
	}

	google := IssuerSpec{Name: "google", PresetType: "google_cloud"}
	result, err := verify(google, func(jwt.MapClaims) {})
	require.NoError(t, err)
	require.Empty(t, result.JTI)
	require.False(t, result.SingleUse)

	entra := IssuerSpec{Name: "entra", IssuerURL: "https://login.microsoftonline.com/11111111-2222-3333-4444-555555555555/v2.0"}
	result, err = verify(entra, func(c jwt.MapClaims) { c["uti"] = "token-a" })
	require.NoError(t, err)
	require.Equal(t, "token-a", result.JTI)
	require.True(t, result.SingleUse)
	_, err = verify(entra, func(jwt.MapClaims) {})
	require.ErrorIs(t, err, ErrAssertion, "a single-use Entra token must carry uti")
	_, err = verify(entra, func(c jwt.MapClaims) { c["jti"] = "token-a" })
	require.ErrorIs(t, err, ErrAssertion, "jti does not stand in for uti")

	entra.TokenValidation.ReplayProtection = ReplayAllowReuse
	result, err = verify(entra, func(jwt.MapClaims) {})
	require.NoError(t, err)
	require.False(t, result.SingleUse)

	// Azure managed identity tokens live slightly over 24 hours.
	limit := 90000
	entra.TokenValidation.MaxLifetimeSeconds = &limit
	_, err = verify(entra, func(c jwt.MapClaims) { c["exp"] = f.now.Add(86700 * time.Second).Unix() })
	require.NoError(t, err)
	_, err = verify(entra, func(c jwt.MapClaims) { c["exp"] = f.now.Add(time.Duration(limit+1) * time.Second).Unix() })
	require.ErrorIs(t, err, ErrAssertion)
}

func TestMalformedAssertionsDoNotFetchKeys(t *testing.T) {
	f := newAssertionFixture(t)
	v := NewVerifier()
	v.now = func() time.Time { return f.now }
	var calls atomic.Int32
	v.fetch = func(context.Context, IssuerSpec) ([]byte, error) { calls.Add(1); return f.keys, nil }
	badClaims := f.claims()
	badClaims["iss"] = "https://unknown.example.test"
	for _, raw := range []string{"not-a-jwt", f.sign(t, badClaims, nil), f.sign(t, f.claims(), map[string]any{"typ": "oauth-wag+jwt"})} {
		_, err := v.Verify(context.Background(), f.trust(t), "https://auth.example.test", raw)
		require.ErrorIs(t, err, ErrAssertion)
	}
	duplicateHeader := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","alg":"RS256","kid":"key-a"}`))
	_, err := v.Verify(context.Background(), f.trust(t), "https://auth.example.test", duplicateHeader+".e30.signature")
	require.ErrorIs(t, err, ErrAssertion)
	require.Zero(t, calls.Load())
}

func TestKeyRotationUnknownKidIsRateLimited(t *testing.T) {
	f := newAssertionFixture(t)
	v := NewVerifier()
	now := f.now
	v.now = func() time.Time { return now }
	var calls int
	keys := f.keys
	v.fetch = func(context.Context, IssuerSpec) ([]byte, error) { calls++; return keys, nil }
	trust := f.trust(t)
	_, err := v.Verify(context.Background(), trust, "https://auth.example.test", f.sign(t, f.claims(), nil))
	require.NoError(t, err)
	for range 3 {
		_, err = v.Verify(context.Background(), trust, "https://auth.example.test", f.sign(t, f.claims(), map[string]any{"kid": "unknown"}))
		require.ErrorIs(t, err, ErrAssertion)
	}
	require.Equal(t, 2, calls)
	keys, err = json.Marshal(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &f.key.PublicKey, KeyID: "new", Algorithm: "RS256"}}})
	require.NoError(t, err)
	now = now.Add(keyRefreshPeriod + time.Second)
	claims := f.claims()
	claims["iat"], claims["exp"] = now.Unix(), now.Add(5*time.Minute).Unix()
	_, err = v.Verify(context.Background(), trust, "https://auth.example.test", f.sign(t, claims, map[string]any{"kid": "new"}))
	require.NoError(t, err)
	require.Equal(t, 3, calls)
}

type observedWaitContext struct {
	context.Context
	once    sync.Once
	waiting chan struct{}
}

func (c *observedWaitContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.waiting) })
	return c.Context.Done()
}

func TestFailedKeyRefreshRetriesAfterTheFailureCooldown(t *testing.T) {
	f := newAssertionFixture(t)
	v := NewVerifier()
	now := f.now
	v.now = func() time.Time { return now }
	keys, failing := f.keys, false
	v.fetch = func(context.Context, IssuerSpec) ([]byte, error) {
		if failing {
			return nil, ErrUnavailable
		}
		return keys, nil
	}
	trust := f.trust(t)
	verify := func(kid string) error {
		claims := f.claims()
		claims["iat"], claims["exp"] = now.Unix(), now.Add(5*time.Minute).Unix()
		_, err := v.Verify(context.Background(), trust, "https://auth.example.test", f.sign(t, claims, map[string]any{"kid": kid}))
		return err
	}
	require.NoError(t, verify("key-a"))
	// The issuer rotates while its key endpoint is briefly down.
	failing = true
	require.ErrorIs(t, verify("new"), ErrUnavailable)
	rotated, err := json.Marshal(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &f.key.PublicKey, KeyID: "new", Algorithm: "RS256"}}})
	require.NoError(t, err)
	keys, failing = rotated, false
	now = now.Add(keyFailureTTL + time.Second)
	require.NoError(t, verify("new"))
}

func TestConcurrentRotationVerificationsJoinTheRefresh(t *testing.T) {
	f := newAssertionFixture(t)
	v := NewVerifier()
	v.now = func() time.Time { return f.now }
	newKeys, err := json.Marshal(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &f.key.PublicKey, KeyID: "new", Algorithm: "RS256"}}})
	require.NoError(t, err)
	started, release := make(chan struct{}), make(chan struct{})
	var unblock sync.Once
	defer unblock.Do(func() { close(release) })
	var calls atomic.Int32
	v.fetch = func(ctx context.Context, _ IssuerSpec) ([]byte, error) {
		if calls.Add(1) == 1 {
			return f.keys, nil
		}
		close(started)
		select {
		case <-release:
			return newKeys, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	trust := f.trust(t)
	_, err = v.Verify(context.Background(), trust, "https://auth.example.test", f.sign(t, f.claims(), nil))
	require.NoError(t, err)
	first, second := make(chan error, 1), make(chan error, 1)
	firstToken := f.sign(t, f.claims(), map[string]any{"kid": "new"})
	secondToken := f.sign(t, f.claims(), map[string]any{"kid": "new"})
	go func() {
		_, err := v.Verify(context.Background(), trust, "https://auth.example.test", firstToken)
		first <- err
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("rotation refresh did not start")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	observer := &observedWaitContext{Context: ctx, waiting: make(chan struct{})}
	go func() { _, err := v.Verify(observer, trust, "https://auth.example.test", secondToken); second <- err }()
	select {
	case <-observer.waiting:
	case err := <-second:
		t.Fatalf("second verification returned before joining the refresh: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("second verification did not join the refresh")
	}
	unblock.Do(func() { close(release) })
	require.NoError(t, <-first)
	require.NoError(t, <-second)
	require.EqualValues(t, 2, calls.Load(), "one initial load and one shared rotation refresh")
}

func TestConcurrentKeyFetchAndCanceledWaiter(t *testing.T) {
	f := newAssertionFixture(t)
	v := NewVerifier()
	v.now = func() time.Time { return f.now }
	started, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	v.fetch = func(ctx context.Context, _ IssuerSpec) ([]byte, error) {
		if calls.Add(1) == 1 {
			close(started)
		}
		select {
		case <-release:
			return f.keys, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	trust := f.trust(t)
	ctx, cancel := context.WithCancel(context.Background())
	first := make(chan error, 1)
	go func() { _, err := v.keySet(ctx, trust, false); first <- err }()
	<-started
	cancel()
	require.ErrorIs(t, <-first, context.Canceled)
	var wg sync.WaitGroup
	results := make(chan error, 10)
	for range 10 {
		wg.Go(func() { _, err := v.keySet(context.Background(), trust, false); results <- err })
	}
	close(release)
	wg.Wait()
	close(results)
	for err := range results {
		require.NoError(t, err)
	}
	require.EqualValues(t, 1, calls.Load())
}

func TestKeyCacheDoesNotServeExpiredKeysOnOutage(t *testing.T) {
	f := newAssertionFixture(t)
	v := NewVerifier()
	now := f.now
	v.now = func() time.Time { return now }
	outage := false
	v.fetch = func(context.Context, IssuerSpec) ([]byte, error) {
		if outage {
			return nil, errors.New("synthetic outage")
		}
		return f.keys, nil
	}
	_, err := v.keySet(context.Background(), f.trust(t), false)
	require.NoError(t, err)
	outage, now = true, now.Add(keyCacheTTL+time.Second)
	_, err = v.keySet(context.Background(), f.trust(t), false)
	require.ErrorIs(t, err, ErrUnavailable)
}

func TestOnlyPublicAsymmetricKeysAreAccepted(t *testing.T) {
	f := newAssertionFixture(t)
	for _, key := range []jose.JSONWebKey{{Key: f.key}, {Key: []byte("symmetric-secret")}} {
		raw, err := json.Marshal(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{key}})
		require.NoError(t, err)
		_, err = parseKeySet(raw)
		require.Error(t, err)
	}
	curve, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	raw, err := json.Marshal(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &curve.PublicKey, KeyID: "ec", Algorithm: "ES256"}}})
	require.NoError(t, err)
	keys, err := parseKeySet(raw)
	require.NoError(t, err)
	require.Len(t, keys, 1)
	edPublic, _, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	raw, err = json.Marshal(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: edPublic, KeyID: "ed", Algorithm: "EdDSA"}}})
	require.NoError(t, err)
	_, err = parseKeySet(raw)
	require.NoError(t, err)
	weakKey := `{"keys":[{"kty":"OKP","crv":"Ed25519","kid":"weak","x":"` + base64.RawURLEncoding.EncodeToString(make([]byte, ed25519.PublicKeySize)) + `"}]}`
	_, err = parseKeySet([]byte(weakKey))
	require.Error(t, err)
	var set map[string][]map[string]any
	require.NoError(t, json.Unmarshal(f.keys, &set))
	key := maps.Clone(set["keys"][0])
	key["p"] = "private-prime"
	raw, err = json.Marshal(map[string]any{"keys": []map[string]any{key}})
	require.NoError(t, err)
	_, err = parseKeySet(raw)
	require.Error(t, err)
}

// OIDC discovery lets an issuer publish signing and encryption keys in one set.
func TestKeySetSkipsKeysForOtherPurposes(t *testing.T) {
	f := newAssertionFixture(t)
	var set map[string][]map[string]any
	require.NoError(t, json.Unmarshal(f.keys, &set))
	signing := set["keys"][0]
	encryption := maps.Clone(signing)
	encryption["use"], encryption["alg"] = "enc", "RSA-OAEP"
	wrapping := maps.Clone(signing)
	delete(wrapping, "use")
	wrapping["kid"], wrapping["key_ops"] = "wrap", []string{"wrapKey"}
	parse := func(entries ...map[string]any) ([]jose.JSONWebKey, error) {
		raw, err := json.Marshal(map[string]any{"keys": entries})
		require.NoError(t, err)
		return parseKeySet(raw)
	}

	keys, err := parse(encryption, signing, wrapping)
	require.NoError(t, err)
	require.Len(t, keys, 1)
	require.Equal(t, "key-a", keys[0].KeyID)
	require.Equal(t, "sig", keys[0].Use)

	_, err = parse(encryption, wrapping)
	require.Error(t, err, "a set without a signing key is unusable")
	both := maps.Clone(signing)
	delete(both, "use")
	both["key_ops"] = []string{"sign", "verify"}
	_, err = parse(both)
	require.Error(t, err, "a signing candidate is still validated strictly")
	_, err = parse(map[string]any{"kty": "RSA", "use": 1})
	require.Error(t, err)
}

func TestDuplicateJSONAndDiscoveryIdentity(t *testing.T) {
	for _, raw := range []string{`{"iss":"a","\u0069ss":"b"}`, `{"x":{"n":1,"n":2}}`, `{} {}`, `[]`} {
		require.False(t, uniqueJSONObject([]byte(raw)), raw)
	}
	require.True(t, uniqueJSONObject([]byte(`{"a":[1,{"b":true}],"c":null}`)))
	_, err := decodeDiscovery([]byte(`{"Issuer":"wrong","jwks_uri":"https://keys.example.test"}`))
	require.Error(t, err)
	metadata, err := decodeDiscovery([]byte(`{"issuer":"https://identity.example.test","jwks_uri":"https://keys.example.test?p=public-policy"}`))
	require.NoError(t, err)
	require.Equal(t, "https://identity.example.test", metadata.Issuer)
}

func TestKeyCacheIsBounded(t *testing.T) {
	f := newAssertionFixture(t)
	v := NewVerifier()
	v.now = func() time.Time { return f.now }
	v.fetch = func(context.Context, IssuerSpec) ([]byte, error) { return f.keys, nil }
	for i := range maxKeyCaches + 8 {
		trust := f.trust(t)
		trust.Name = fmt.Sprintf("issuer-%d", i)
		_, err := v.keySet(context.Background(), trust, false)
		require.NoError(t, err)
	}
	require.LessOrEqual(t, len(v.cache), maxKeyCaches)
}
