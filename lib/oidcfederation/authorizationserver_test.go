package oidcfederation

import (
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/golang-jwt/jwt/v5"
	"github.com/stretchr/testify/require"
)

var accessTokenTypes = []string{"at+jwt", "application/at+jwt"}

type authorizationServerFixture struct {
	now  time.Time
	spec IssuerSpec
	keys map[string]*ecdsa.PrivateKey
}

func newAuthorizationServerFixture(t *testing.T) *authorizationServerFixture {
	t.Helper()
	spec, err := AuthorizationServer("https://auth.example.test", KeySource{})
	require.NoError(t, err)
	return &authorizationServerFixture{now: time.Unix(1700000000, 0), spec: spec, keys: map[string]*ecdsa.PrivateKey{}}
}

// keySet generates the named keys that the fixture does not hold yet and
// returns the key set document publishing all of the named ones.
func (f *authorizationServerFixture) keySet(t *testing.T, ids ...string) []byte {
	t.Helper()
	var set jose.JSONWebKeySet
	for _, id := range ids {
		if f.keys[id] == nil {
			key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
			require.NoError(t, err)
			f.keys[id] = key
		}
		set.Keys = append(set.Keys, jose.JSONWebKey{Key: &f.keys[id].PublicKey, KeyID: id, Algorithm: "ES256", Use: "sig"})
	}
	raw, err := json.Marshal(set)
	require.NoError(t, err)
	return raw
}

func (f *authorizationServerFixture) token(t *testing.T, keyID, typ string) string {
	t.Helper()
	token := jwt.NewWithClaims(jwt.SigningMethodES256, jwt.MapClaims{"iss": f.spec.IssuerURL, "sub": "federation:ci", "aud": "prod", "iat": f.now.Unix(), "exp": f.now.Add(24 * time.Hour).Unix()})
	token.Header["kid"], token.Header["typ"] = keyID, typ
	raw, err := token.SignedString(f.keys[keyID])
	require.NoError(t, err)
	return raw
}

type memoryKeyStore struct {
	mu    sync.Mutex
	saved map[string][]byte
	loads int
}

func (s *memoryKeyStore) Save(id, _ string, keySet []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.saved == nil {
		s.saved = map[string][]byte{}
	}
	s.saved[id] = keySet
}

func (s *memoryKeyStore) Load(id string) ([]byte, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.loads++
	keySet, ok := s.saved[id]
	return keySet, ok
}

func TestVerifySignatureAppliesOnlyTheGivenTokenTypes(t *testing.T) {
	f := newAuthorizationServerFixture(t)
	v := NewVerifier()
	v.now = func() time.Time { return f.now }
	keys := f.keySet(t, "key-a")
	v.fetch = func(context.Context, IssuerSpec) ([]byte, error) { return keys, nil }

	claims, err := v.VerifySignature(context.Background(), f.spec, f.token(t, "key-a", "at+jwt"), accessTokenTypes)
	require.NoError(t, err)
	require.Equal(t, "federation:ci", claims["sub"])
	require.Equal(t, "prod", claims["aud"], "the audience is the caller's to judge")

	for _, typ := range []string{"JWT", "", "dpop+jwt"} {
		_, err = v.VerifySignature(context.Background(), f.spec, f.token(t, "key-a", typ), accessTokenTypes)
		require.ErrorIs(t, err, ErrAssertion, typ)
	}
	// An access token is not an identity assertion either.
	_, err = v.VerifySignature(context.Background(), f.spec, f.token(t, "key-a", "at+jwt"), assertionTypes)
	require.ErrorIs(t, err, ErrAssertion)
}

func TestAuthorizationServerMetadataFollowsRFC8414(t *testing.T) {
	for issuer, metadata := range map[string]string{
		"https://auth.example.test":          "https://auth.example.test/.well-known/oauth-authorization-server",
		"https://auth.example.test/":         "https://auth.example.test/.well-known/oauth-authorization-server",
		"https://auth.example.test/tenant-1": "https://auth.example.test/.well-known/oauth-authorization-server/tenant-1",
	} {
		spec, err := AuthorizationServer(issuer, KeySource{})
		require.NoError(t, err)
		require.Equal(t, "discovery", spec.JWKS.Type)
		require.Equal(t, metadata, metadataURL(spec))
	}
	identity, err := normalizeIssuer(IssuerSpec{Name: "idp", IssuerURL: "https://auth.example.test/tenant-1"})
	require.NoError(t, err)
	require.Equal(t, "https://auth.example.test/tenant-1/.well-known/openid-configuration", metadataURL(identity))

	_, err = AuthorizationServer("https://auth.example.test", KeySource{DiscoveryBaseURL: "https://metadata.example.test"})
	require.ErrorIs(t, err, ErrInvalid)
	_, err = AuthorizationServer(GitHubIssuer, KeySource{})
	require.ErrorIs(t, err, ErrInvalid, "an identity provider with its own rules is not an authorization server")
}

// A resource server checks every request against these keys, so the last key
// set keeps answering while the authorization server cannot be reached.
func TestAuthorizationServerKeysStayInUseWhileARefreshFails(t *testing.T) {
	f := newAuthorizationServerFixture(t)
	v := NewVerifier()
	v.now = func() time.Time { return f.now }
	var fetches atomic.Int32
	var mu sync.Mutex
	published, failing := f.keySet(t, "key-a"), false
	v.fetch = func(context.Context, IssuerSpec) ([]byte, error) {
		fetches.Add(1)
		mu.Lock()
		defer mu.Unlock()
		if failing {
			return nil, ErrUnavailable
		}
		return published, nil
	}
	verify := func(keyID string) error {
		_, err := v.VerifySignature(context.Background(), f.spec, f.token(t, keyID, "at+jwt"), accessTokenTypes)
		return err
	}
	require.NoError(t, verify("key-a"))
	require.EqualValues(t, 1, fetches.Load())

	// The cached set expires while the server is down.
	mu.Lock()
	failing = true
	mu.Unlock()
	f.now = f.now.Add(keyCacheTTL + time.Second)
	require.NoError(t, verify("key-a"), "answered from the last key set without waiting for the refresh")
	require.Eventually(t, func() bool { return fetches.Load() == 2 }, time.Second, time.Millisecond, "a refresh runs in the background")
	require.Eventually(t, func() bool { v.mu.Lock(); defer v.mu.Unlock(); return len(v.loading) == 0 }, time.Second, time.Millisecond)
	require.NoError(t, verify("key-a"))
	require.EqualValues(t, 2, fetches.Load(), "a failed fetch is not repeated at once")

	// A key the last set lacks cannot be vouched for by it.
	f.keySet(t, "key-b")
	require.ErrorIs(t, verify("key-b"), ErrUnavailable)

	// The server returns, publishing a new key beside the old one.
	mu.Lock()
	published, failing = f.keySet(t, "key-a", "key-b"), false
	mu.Unlock()
	f.now = f.now.Add(keyFailureTTL + time.Second)
	require.NoError(t, verify("key-b"))
	require.NoError(t, verify("key-a"))
}

// The saved copy answers the first requests after a start without waiting for
// the server, which is fetched from at once.
// A token naming a key the last set lacks may arrive while a refresh that
// nobody waits for is running. It waits for that refresh, and when the refresh
// fails the token was not checked against anything newer: the keys are
// unavailable for it, it is not an invalid token.
func TestUnknownKeyIDSharesTheOutcomeOfARunningRefresh(t *testing.T) {
	f := newAuthorizationServerFixture(t)
	v := NewVerifier()
	v.now = func() time.Time { return f.now }
	published := f.keySet(t, "key-a")
	f.keySet(t, "key-b")
	blocked := make(chan struct{})
	release := sync.OnceFunc(func() { close(blocked) })
	t.Cleanup(release)
	var fetches atomic.Int32
	v.fetch = func(context.Context, IssuerSpec) ([]byte, error) {
		if fetches.Add(1) == 1 {
			return published, nil
		}
		<-blocked
		return nil, ErrUnavailable
	}
	verify := func(keyID string) error {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, err := v.VerifySignature(ctx, f.spec, f.token(t, keyID, "at+jwt"), accessTokenTypes)
		return err
	}
	require.NoError(t, verify("key-a"))

	// The cached set expires; the next known token starts a refresh and does not wait.
	f.now = f.now.Add(keyCacheTTL + time.Second)
	require.NoError(t, verify("key-a"))
	require.Eventually(t, func() bool { return fetches.Load() == 2 }, time.Second, time.Millisecond)

	unknown := make(chan error, 1)
	go func() { unknown <- verify("key-b") }()
	select {
	case err := <-unknown:
		t.Fatalf("the token did not wait for the refresh: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	release()
	require.ErrorIs(t, <-unknown, ErrUnavailable)
	require.EqualValues(t, 2, fetches.Load(), "it joined the running refresh")
	require.NoError(t, verify("key-a"), "the last key set still answers for its own keys")
}

func TestAuthorizationServerKeysSurviveARestartThroughTheStore(t *testing.T) {
	f := newAuthorizationServerFixture(t)
	published := f.keySet(t, "key-a")
	store := &memoryKeyStore{}
	token := f.token(t, "key-a", "at+jwt")
	unreachable := func(context.Context, IssuerSpec) ([]byte, error) { return nil, ErrUnavailable }
	idle := func(v *Verifier) bool { v.mu.Lock(); defer v.mu.Unlock(); return len(v.loading) == 0 }

	first := NewVerifierWithKeyStore(store, nil)
	first.now = func() time.Time { return f.now }
	first.fetch = func(context.Context, IssuerSpec) ([]byte, error) { return published, nil }
	_, err := first.VerifySignature(context.Background(), f.spec, token, accessTokenTypes)
	require.NoError(t, err)
	require.Contains(t, store.saved, keyCacheKey(f.spec))
	require.NotContains(t, string(store.saved[keyCacheKey(f.spec)]), `"d"`, "only public keys are kept")
	require.Equal(t, 1, store.loads, "a copy was looked for; there was none, so the first request waited for the fetch")

	// A new process verifies with the copy while its own fetch is still running.
	restarted := NewVerifierWithKeyStore(store, nil)
	restarted.now = func() time.Time { return f.now }
	blocked := make(chan struct{})
	release := sync.OnceFunc(func() { close(blocked) })
	t.Cleanup(release)
	var fetches atomic.Int32
	restarted.fetch = func(context.Context, IssuerSpec) ([]byte, error) {
		fetches.Add(1)
		<-blocked
		return nil, ErrUnavailable
	}
	// A request that waited for the fetch would end with this deadline.
	waiting, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for range 3 {
		_, err = restarted.VerifySignature(waiting, f.spec, token, accessTokenTypes)
		require.NoError(t, err)
	}
	require.Eventually(t, func() bool { return fetches.Load() == 1 }, time.Second, time.Millisecond, "one fetch was started in the background")
	release()
	require.Eventually(t, func() bool { return idle(restarted) }, time.Second, time.Millisecond)
	_, err = restarted.VerifySignature(context.Background(), f.spec, token, accessTokenTypes)
	require.NoError(t, err, "the copy stays in use after the fetch failed")
	require.Equal(t, 2, store.loads, "each process reads the copy once")

	// A copy fetched from one key source does not stand in for another.
	moved, err := AuthorizationServer(f.spec.IssuerURL, KeySource{Type: "explicit_url", URL: "https://keys.example.test/jwks.json"})
	require.NoError(t, err)
	restarted.fetch = unreachable
	_, err = restarted.VerifySignature(context.Background(), moved, token, accessTokenTypes)
	require.ErrorIs(t, err, ErrUnavailable)

	// Without a usable copy there is nothing to verify with.
	for _, v := range []*Verifier{
		NewVerifier(),
		NewVerifierWithKeyStore(&memoryKeyStore{}, nil),
		NewVerifierWithKeyStore(&memoryKeyStore{saved: map[string][]byte{keyCacheKey(f.spec): []byte(`{"keys":`)}}, nil),
	} {
		v.now = func() time.Time { return f.now }
		v.fetch = unreachable
		_, err = v.VerifySignature(context.Background(), f.spec, token, accessTokenTypes)
		require.ErrorIs(t, err, ErrUnavailable)
	}
}

// A server that answers with a key set holding no signing key has withdrawn
// its keys. That is not a failed fetch: the last key set must not outlive it,
// in memory or in the saved copy.
func TestAuthorizationServerCanWithdrawEveryKey(t *testing.T) {
	f := newAuthorizationServerFixture(t)
	store := &memoryKeyStore{}
	published := f.keySet(t, "key-a")
	token := f.token(t, "key-a", "at+jwt")
	v := NewVerifierWithKeyStore(store, nil)
	v.now = func() time.Time { return f.now }
	var mu sync.Mutex
	v.fetch = func(context.Context, IssuerSpec) ([]byte, error) {
		mu.Lock()
		defer mu.Unlock()
		return published, nil
	}
	verify := func(v *Verifier) error {
		_, err := v.VerifySignature(context.Background(), f.spec, token, accessTokenTypes)
		return err
	}
	require.NoError(t, verify(v))

	// A valid public key published for encryption only, as a JWK.
	encryptionKey, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	encryption, err := json.Marshal(jose.JSONWebKey{Key: &encryptionKey.PublicKey, KeyID: "enc-1", Algorithm: "RSA-OAEP", Use: "enc"})
	require.NoError(t, err)
	unsigning := strings.Replace(string(encryption), `"use":"enc"`, `"key_ops":["encrypt","wrapKey"]`, 1)
	require.NotEqual(t, string(encryption), unsigning)

	for _, withdrawn := range []string{`{"keys":[]}`, `{"keys":[` + string(encryption) + `]}`, `{"keys":[` + unsigning + `]}`} {
		mu.Lock()
		published = []byte(withdrawn)
		mu.Unlock()
		f.now = f.now.Add(keyCacheTTL + keyRefreshPeriod + time.Second)
		// The first request after the cache expires is still answered from the
		// last key set; the refresh it starts removes that set.
		_ = verify(v)
		require.Eventually(t, func() bool { v.mu.Lock(); defer v.mu.Unlock(); return len(v.loading) == 0 }, time.Second, time.Millisecond)
		require.ErrorIs(t, verify(v), ErrAssertion, withdrawn)
		require.JSONEq(t, `{"keys":[]}`, string(store.saved[keyCacheKey(f.spec)]), "the saved copy is replaced too")

		// A restart does not bring the withdrawn key back, reachable or not.
		// Which refusal it is depends on whether the failed fetch has ended.
		restarted := NewVerifierWithKeyStore(store, nil)
		restarted.now = func() time.Time { return f.now }
		restarted.fetch = func(context.Context, IssuerSpec) ([]byte, error) { return nil, ErrUnavailable }
		require.Error(t, verify(restarted), withdrawn)
		require.Eventually(t, func() bool { restarted.mu.Lock(); defer restarted.mu.Unlock(); return len(restarted.loading) == 0 }, time.Second, time.Millisecond)
		require.Error(t, verify(restarted), withdrawn)

		// Publishing the key again restores it.
		mu.Lock()
		published = f.keySet(t, "key-a")
		mu.Unlock()
		f.now = f.now.Add(keyCacheTTL + keyRefreshPeriod + time.Second)
		require.NoError(t, verify(v))
	}

	// A response that is not a key set at all is a failed fetch, and the last
	// key set stays.
	for _, broken := range []string{
		`{"keys":null}`, `{}`, `not json`,
		`{"keys":[{"kty":"RSA","use":"sig","kid":"bad"}]}`,
		// Entries that are skipped but are not valid public keys say nothing
		// about the server's keys.
		`{"keys":[{"use":"enc"}]}`,
		`{"keys":[{"kty":"oct","use":"enc","kid":"secret"}]}`,
		`{"keys":[{"kty":"RSA","use":"enc","e":"AQAB"}]}`,
		`{"keys":[{"kty":"RSA","use":"enc","n":"not base64url!","e":"AQAB"}]}`,
		`{"keys":[{"kty":"RSA","use":"enc","n":"AQAB","e":"AQAB"}]}`,
		`{"keys":[{"kty":"EC","use":"enc","crv":"P-256","x":"AQAB","y":"AQAB"}]}`,
		`{"keys":[{"kty":"OKP","use":"enc","crv":"X25519","x":"hSDwCYkwp1R0i33ctD73Wg2_Og0mOBr066SpjqqbTmo"}]}`,
		`{"keys":[` + strings.Replace(string(encryption), `"use":"enc"`, `"key_ops":null`, 1) + `]}`,
		`{"keys":[` + strings.Replace(string(encryption), `"use":"enc"`, `"use":"enc","key_ops":null`, 1) + `]}`,
		`{"keys":[` + strings.Replace(string(encryption), `"use":"enc"`, `"use":"enc","key_ops":["verify"]`, 1) + `]}`,
		`{"keys":[` + strings.Replace(string(encryption), `"use":"enc"`, `"use":"enc","d":"AQAB"`, 1) + `]}`,
		`{"keys":[` + string(encryption) + `,{"use":"enc"}]}`,
	} {
		mu.Lock()
		published = []byte(broken)
		mu.Unlock()
		f.now = f.now.Add(keyCacheTTL + keyRefreshPeriod + time.Second)
		require.NoError(t, verify(v), broken)
		require.Eventually(t, func() bool { v.mu.Lock(); defer v.mu.Unlock(); return len(v.loading) == 0 }, time.Second, time.Millisecond)
		require.NoError(t, verify(v), broken)
	}

	// An identity issuer that publishes no signing key is misconfigured, as before.
	identity, err := normalizeIssuer(IssuerSpec{Name: "idp", IssuerURL: f.spec.IssuerURL})
	require.NoError(t, err)
	empty := NewVerifier()
	empty.now = func() time.Time { return f.now }
	empty.fetch = func(context.Context, IssuerSpec) ([]byte, error) { return []byte(`{"keys":[]}`), nil }
	_, err = empty.VerifySignature(context.Background(), identity, f.token(t, "key-a", "JWT"), assertionTypes)
	var problem *KeyProblem
	require.ErrorAs(t, err, &problem)
}

// An identity issuer's keys are neither kept past their lifetime nor stored:
// only an authorization server's are.
func TestIdentityIssuerKeysAreNotKept(t *testing.T) {
	f := newAuthorizationServerFixture(t)
	published := f.keySet(t, "key-a")
	identity, err := normalizeIssuer(IssuerSpec{Name: "idp", IssuerURL: f.spec.IssuerURL})
	require.NoError(t, err)
	store := &memoryKeyStore{}
	v := NewVerifierWithKeyStore(store, nil)
	v.now = func() time.Time { return f.now }
	failing := false
	v.fetch = func(context.Context, IssuerSpec) ([]byte, error) {
		if failing {
			return nil, errors.New("down")
		}
		return published, nil
	}
	token := f.token(t, "key-a", "JWT")
	_, err = v.VerifySignature(context.Background(), identity, token, assertionTypes)
	require.NoError(t, err)
	require.Empty(t, store.saved)

	failing = true
	f.now = f.now.Add(keyCacheTTL + time.Second)
	_, err = v.VerifySignature(context.Background(), identity, token, assertionTypes)
	require.ErrorIs(t, err, ErrUnavailable)
	require.Zero(t, store.loads)
}

// The JWT library checks an ECDSA signature's width against the algorithm
// and verifies it on the key's own curve. A token must not name one curve's
// algorithm while being signed on another, or the rest of the server would
// reason about its signature on the wrong curve.
func TestKeyMustBeOnTheCurveOfTheAlgorithm(t *testing.T) {
	f := newAuthorizationServerFixture(t)
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	// Published without an algorithm, so only its curve can rule out ES512.
	published, err := json.Marshal(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &key.PublicKey, KeyID: "key-a", Use: "sig"}}})
	require.NoError(t, err)
	v := NewVerifier()
	v.now = func() time.Time { return f.now }
	v.fetch = func(context.Context, IssuerSpec) ([]byte, error) { return published, nil }

	encode := base64.RawURLEncoding.EncodeToString
	payload, err := json.Marshal(map[string]any{"iss": f.spec.IssuerURL, "sub": "federation:ci", "aud": "prod", "iat": f.now.Unix(), "exp": f.now.Add(time.Hour).Unix()})
	require.NoError(t, err)
	sign := func(algorithm string, width int) string {
		input := encode([]byte(`{"alg":"`+algorithm+`","kid":"key-a","typ":"at+jwt"}`)) + "." + encode(payload)
		hash := sha256.Sum256([]byte(input))
		digest := hash[:]
		if algorithm == "ES512" {
			long := sha512.Sum512([]byte(input))
			digest = long[:]
		}
		r, s, err := ecdsa.Sign(rand.Reader, key, digest)
		require.NoError(t, err)
		signature := make([]byte, 2*width)
		r.FillBytes(signature[:width])
		s.FillBytes(signature[width:])
		return input + "." + encode(signature)
	}

	_, err = v.VerifySignature(context.Background(), f.spec, sign("ES256", 32), accessTokenTypes)
	require.NoError(t, err)

	mislabelled := sign("ES512", 66)
	_, err = jwt.Parse(mislabelled, func(*jwt.Token) (any, error) { return &key.PublicKey, nil }, jwt.WithValidMethods([]string{"ES512"}), jwt.WithTimeFunc(func() time.Time { return f.now }))
	require.NoError(t, err, "the library alone accepts it, otherwise the test proves nothing")
	_, err = v.VerifySignature(context.Background(), f.spec, mislabelled, accessTokenTypes)
	require.ErrorIs(t, err, ErrAssertion)
}

func TestKeyFitsItsAlgorithm(t *testing.T) {
	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	p384, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	require.NoError(t, err)
	edKey, _, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	fits := map[string][]string{}
	for _, algorithm := range []string{"RS256", "RS384", "RS512", "PS256", "PS384", "PS512", "ES256", "ES384", "ES512", "EdDSA", "HS256", "none"} {
		for name, key := range map[string]any{"rsa": &rsaKey.PublicKey, "p384": &p384.PublicKey, "ed25519": edKey, "secret": []byte("secret")} {
			if keyFits(key, algorithm) {
				fits[name] = append(fits[name], algorithm)
			}
		}
	}
	require.Equal(t, map[string][]string{"rsa": {"RS256", "RS384", "RS512", "PS256", "PS384", "PS512"}, "p384": {"ES384"}, "ed25519": {"EdDSA"}}, fits)
}

// A key set with no signing key withdraws the server's keys only when every
// entry is a valid public key whose usage plainly names another purpose.
// Anything less certain is a failed fetch.
func TestOnlyValidKeysForAnotherPurposeWithdraw(t *testing.T) {
	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	raw, err := json.Marshal(jose.JSONWebKey{Key: &rsaKey.PublicKey, KeyID: "key-1"})
	require.NoError(t, err)
	// entry is the public key with the given usage members added.
	entry := func(usage string) string {
		return strings.Replace(string(raw), "{", "{"+usage+",", 1)
	}
	set := func(entries ...string) []byte { return []byte(`{"keys":[` + strings.Join(entries, ",") + `]}`) }

	for _, usage := range []string{
		`"use":"enc"`,
		`"key_ops":["encrypt"]`,
		`"key_ops":["encrypt","wrapKey"]`,
		`"use":"enc","key_ops":["encrypt","wrapKey"]`,
		`"use":"enc","key_ops":[]`,
		`"key_ops":["decrypt","unwrapKey","deriveKey","deriveBits"]`,
	} {
		_, err := parseKeySet(set(entry(usage)))
		require.ErrorIs(t, err, errNoSigningKeys, usage)
	}
	_, err = parseKeySet(set())
	require.ErrorIs(t, err, errNoSigningKeys)

	for _, usage := range []string{
		// The usage is missing, malformed, misspelt or says nothing.
		`"use":""`,
		`"use":"Sig"`,
		`"use":"encryption"`,
		`"key_ops":["Verify"]`,
		`"key_ops":["encrypt","unknown"]`,
		`"key_ops":[]`,
		`"key_ops":null`,
		`"use":"enc","key_ops":null`,
		`"use":"enc","key_ops":"encrypt"`,
		`"use":"enc","key_ops":[1]`,
		// The usage contradicts itself, or is for signatures after all.
		`"use":"enc","key_ops":["verify"]`,
		`"use":"enc","key_ops":["sign"]`,
		`"use":"enc","key_ops":["encrypt","verify"]`,
		`"use":"sig","key_ops":["encrypt"]`,
		`"use":"sig","key_ops":["sign"]`,
		`"key_ops":["sign"]`,
	} {
		_, err := parseKeySet(set(entry(usage)))
		require.ErrorIs(t, err, ErrInvalid, usage)
		require.NotErrorIs(t, err, errNoSigningKeys, usage)
	}

	// None of this changes which keys a mixed set yields: the signing key is
	// used and the other entry is passed over, whatever its usage says.
	signing := strings.Replace(entry(`"use":"sig"`), `"kid":"key-1"`, `"kid":"key-2"`, 1)
	for _, usage := range []string{`"use":"enc"`, `"use":"enc","key_ops":null`, `"use":"enc","key_ops":["verify"]`, `"key_ops":["sign"]`, `"key_ops":null`, `"use":""`} {
		keys, err := parseKeySet(set(entry(usage), signing))
		require.NoError(t, err, usage)
		require.Len(t, keys, 1, usage)
		require.Equal(t, "key-2", keys[0].KeyID)
	}
}
