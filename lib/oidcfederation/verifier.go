package oidcfederation

import (
	"bytes"
	"cmp"
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/golang-jwt/jwt/v5"
)

const (
	keyCacheTTL      = 5 * time.Minute
	keyFailureTTL    = 10 * time.Second
	keyRefreshPeriod = time.Minute
	maxKeyCaches     = 128
	maxKeyLoads      = 64
	// clockLeeway is how far the issuer's clock may differ from this host's.
	clockLeeway = 30 * time.Second
)

// Assertion contains verified identity claims, never permissions inferred from those claims.
type Assertion struct {
	Subject   string
	JTI       string
	IssuedAt  time.Time
	ExpiresAt time.Time
	Claims    map[string]any
	SingleUse bool
}

type cachedKeys struct {
	keys         []jose.JSONWebKey
	expiresAt    time.Time
	failureUntil time.Time
	refreshAfter time.Time
	// failure is what the last load reported, returned again until failureUntil.
	failure error
	// restored is set once the stored copy of an authorization server's key
	// set has been looked for.
	restored bool
}

type keyLoad struct {
	done chan struct{}
	keys []jose.JSONWebKey
	err  error
}

// KeyStore keeps the last key set fetched from an authorization server and
// the time of that fetch, so that a restarted server continues with the cache
// it had.
// A copy that is missing or unreadable is simply not used. The id names one
// issuer URL together with its configured key source, so a copy is not reused
// after either changes; the issuer URL is for the store's own reporting.
type KeyStore interface {
	Save(id, issuerURL string, keySet []byte)
	Load(id string) ([]byte, bool)
}

// Verifier bounds issuer key fetching and validates the supported token profiles.
type Verifier struct {
	mu      sync.Mutex
	cache   map[string]cachedKeys
	loading map[string]*keyLoad
	fetch   func(context.Context, IssuerSpec) ([]byte, error)
	now     func() time.Time
	store   KeyStore
	log     *slog.Logger
}

// NewVerifier uses HTTPS public-address-only key retrieval and an in-memory bounded cache.
func NewVerifier() *Verifier {
	return &Verifier{cache: make(map[string]cachedKeys), loading: make(map[string]*keyLoad), fetch: fetchIssuerKeys, now: time.Now, log: slog.New(slog.DiscardHandler)}
}

// NewVerifierWithKeyStore is NewVerifier for a resource server: store keeps
// the key sets of authorization servers between starts, and log is told when
// one of them cannot be refreshed. Either may be nil.
func NewVerifierWithKeyStore(store KeyStore, log *slog.Logger) *Verifier {
	v := NewVerifier()
	v.store = store
	if log != nil {
		v.log = log
	}
	return v
}

func keyCacheKey(issuer IssuerSpec) string {
	raw, _ := json.Marshal(struct {
		Name   string
		Issuer string
		Source KeySource
	}{issuer.Name, issuer.IssuerURL, issuer.JWKS})
	digest := sha256.Sum256(raw)
	return hex.EncodeToString(digest[:])
}

func (v *Verifier) putLocked(key string, entry cachedKeys) {
	if _, exists := v.cache[key]; !exists && len(v.cache) >= maxKeyCaches {
		for oldKey := range v.cache {
			delete(v.cache, oldKey)
			break
		}
	}
	v.cache[key] = entry
}

func (v *Verifier) keySet(ctx context.Context, issuer IssuerSpec, force bool) ([]jose.JSONWebKey, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	key := keyCacheKey(issuer)
	now := v.now()
	v.mu.Lock()
	entry := v.cache[key]
	if issuer.authorizationServer && !entry.restored {
		entry = v.restoreLocked(key, issuer, entry, now)
	}
	if now.Before(entry.expiresAt) && !force {
		v.mu.Unlock()
		return entry.keys, nil
	}
	// An authorization server's last key set keeps answering while a refresh
	// runs or fails. Only a key ID it does not hold waits for one, and shares
	// that refresh's outcome: the last key set is no answer for it.
	retained := issuer.authorizationServer && !force && entry.keys != nil
	load, loading := v.loading[key]
	if !loading {
		switch {
		case now.Before(entry.failureUntil) && retained, len(v.loading) >= maxKeyLoads && retained:
			v.mu.Unlock()
			return entry.keys, nil
		case now.Before(entry.failureUntil):
			v.mu.Unlock()
			return nil, cmp.Or(entry.failure, ErrUnavailable)
		case force && now.Before(entry.refreshAfter):
			v.mu.Unlock()
			return entry.keys, nil
		case len(v.loading) >= maxKeyLoads:
			v.mu.Unlock()
			return nil, ErrUnavailable
		}
		if force {
			entry.refreshAfter = now.Add(keyRefreshPeriod)
			v.putLocked(key, entry)
		}
		load = &keyLoad{done: make(chan struct{})}
		v.loading[key] = load
		go v.loadKeys(key, issuer, load, now)
	}
	v.mu.Unlock()
	if retained {
		return entry.keys, nil
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-load.done:
		return load.keys, load.err
	}
}

// savedKeySet is an authorization server's key set as a KeyStore keeps it:
// the public keys and when they were fetched, which is what a cache entry
// consists of.
type savedKeySet struct {
	FetchedAt time.Time         `json:"fetched_at"`
	Keys      []jose.JSONWebKey `json:"keys"`
}

// restoreLocked continues an authorization server's cache entry from the
// stored copy of its key set, as if the server had not restarted: a copy
// fetched within the cache lifetime is used until that lifetime ends, and an
// older one is used while the set is fetched again. The copy is read with the
// lock held, once for each issuer.
func (v *Verifier) restoreLocked(key string, issuer IssuerSpec, entry cachedKeys, now time.Time) cachedKeys {
	entry.restored = true
	if v.store != nil && issuer.JWKS.Type != "inline" && entry.keys == nil {
		if copied, ok := v.store.Load(key); ok {
			if keys, err := parseAuthorizationServerKeySet(copied); err == nil {
				entry.keys = keys
				// A fetch time that is missing, or later than now because a
				// clock was wrong, leaves the copy due for a refresh at once.
				var saved struct {
					FetchedAt time.Time `json:"fetched_at"`
				}
				if json.Unmarshal(copied, &saved) == nil && !saved.FetchedAt.IsZero() && !saved.FetchedAt.After(now) {
					entry.expiresAt = saved.FetchedAt.Add(keyCacheTTL)
				}
				v.log.Info("authorization server keys restored from the saved copy", "issuer", issuer.IssuerURL, "fetched_at", saved.FetchedAt, "refresh_due", !now.Before(entry.expiresAt))
			}
		}
	}
	v.putLocked(key, entry)
	return entry
}

// parseAuthorizationServerKeySet reads a key set in which no signing key is
// an answer, not an error: the server has withdrawn every key it had. The
// result is then empty and not nil.
func parseAuthorizationServerKeySet(raw []byte) ([]jose.JSONWebKey, error) {
	keys, err := parseKeySet(raw)
	if errors.Is(err, errNoSigningKeys) {
		return []jose.JSONWebKey{}, nil
	}
	return keys, err
}

func (v *Verifier) loadKeys(key string, issuer IssuerSpec, load *keyLoad, startedAt time.Time) {
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	raw, err := v.fetch(ctx, issuer)
	var keys []jose.JSONWebKey
	if err == nil {
		if issuer.authorizationServer {
			keys, err = parseAuthorizationServerKeySet(raw)
		} else {
			keys, err = parseKeySet(raw)
		}
		if err != nil {
			err = &KeyProblem{"the key set is not valid or holds an unsupported key"}
		}
	}
	if err == nil && v.store != nil && issuer.authorizationServer && issuer.JWKS.Type != "inline" {
		if public, marshalErr := json.Marshal(savedKeySet{FetchedAt: startedAt.UTC(), Keys: keys}); marshalErr == nil {
			v.store.Save(key, issuer.IssuerURL, public)
		}
	}
	// Deferred first, so that it runs after the lock is released.
	var failure error
	lastKeySetInUse := false
	defer func() {
		if failure != nil && issuer.authorizationServer {
			v.log.Warn("authorization server keys could not be fetched", "issuer", issuer.IssuerURL, "error", failure, "last_key_set_in_use", lastKeySetInUse)
		}
	}()
	v.mu.Lock()
	defer v.mu.Unlock()
	entry := v.cache[key]
	if err != nil {
		entry.failureUntil = v.now().Add(keyFailureTTL)
		// A refresh that failed did not use up the refresh allowance.
		entry.refreshAfter = time.Time{}
		// Only a problem the operator can fix keeps its reason.
		var problem *KeyProblem
		if load.err = ErrUnavailable; errors.As(err, &problem) {
			load.err = problem
		}
		entry.failure = load.err
		failure, lastKeySetInUse = load.err, entry.keys != nil
	} else {
		entry.keys = keys
		entry.expiresAt = startedAt.Add(keyCacheTTL)
		entry.failureUntil, entry.failure = time.Time{}, nil
		load.keys = keys
	}
	v.putLocked(key, entry)
	delete(v.loading, key)
	close(load.done)
}

// assertionTypes are the `typ` headers an identity assertion may carry. It may
// also carry none.
var assertionTypes = []string{"", "JWT", "application/jwt"}

// verifySigned checks a token's shape, type, issuer and algorithm before any
// key is fetched, then its signature and its time claims.
func (v *Verifier) verifySigned(ctx context.Context, spec IssuerSpec, raw string, types []string, options ...jwt.ParserOption) (jwt.MapClaims, error) {
	if spec.Name == "" || len(raw) > MaxAssertionBytes {
		return nil, ErrAssertion
	}
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return nil, ErrAssertion
	}
	header, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil || !uniqueJSONObject(header) {
		return nil, ErrAssertion
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil || !uniqueJSONObject(payload) {
		return nil, ErrAssertion
	}
	var protected struct{ Algorithm, KeyID, Type string }
	var fields map[string]json.RawMessage
	if json.Unmarshal(header, &fields) != nil {
		return nil, ErrAssertion
	}
	if _, critical := fields["crit"]; critical {
		return nil, ErrAssertion
	}
	for name, destination := range map[string]*string{"alg": &protected.Algorithm, "kid": &protected.KeyID, "typ": &protected.Type} {
		if value, exists := fields[name]; exists {
			value = bytes.TrimSpace(value)
			if len(value) == 0 || value[0] != '"' || json.Unmarshal(value, destination) != nil {
				return nil, ErrAssertion
			}
		}
	}
	if len(protected.KeyID) > 256 || !slices.ContainsFunc(types, func(allowed string) bool { return strings.EqualFold(allowed, protected.Type) }) {
		return nil, ErrAssertion
	}
	var unverified map[string]json.RawMessage
	if json.Unmarshal(payload, &unverified) != nil {
		return nil, ErrAssertion
	}
	var claimedIssuer string
	if json.Unmarshal(unverified["iss"], &claimedIssuer) != nil || claimedIssuer != spec.IssuerURL {
		return nil, ErrAssertion
	}
	algorithms := []string{"RS256", "RS384", "RS512", "PS256", "PS384", "PS512", "ES256", "ES384", "ES512", "EdDSA"}
	if spec.IssuerURL == GitHubIssuer || spec.IssuerURL == GoogleIssuer || spec.PresetType == "microsoft_entra" {
		algorithms = []string{"RS256"}
	}
	if !slices.Contains(algorithms, protected.Algorithm) {
		return nil, ErrAssertion
	}
	keys, err := v.keySet(ctx, spec, false)
	if err != nil {
		return nil, err
	}
	key, ok := matchingKey(keys, protected.KeyID, protected.Algorithm)
	if !ok {
		keys, err = v.keySet(ctx, spec, true)
		if err != nil {
			return nil, err
		}
		key, ok = matchingKey(keys, protected.KeyID, protected.Algorithm)
		if !ok {
			return nil, ErrAssertion
		}
	}
	claims := jwt.MapClaims{}
	options = append([]jwt.ParserOption{jwt.WithValidMethods(algorithms), jwt.WithIssuer(spec.IssuerURL), jwt.WithExpirationRequired(), jwt.WithIssuedAt(), jwt.WithLeeway(clockLeeway), jwt.WithTimeFunc(v.now), jwt.WithJSONNumber()}, options...)
	token, err := jwt.ParseWithClaims(raw, claims, func(*jwt.Token) (any, error) { return key.Key, nil }, options...)
	if err != nil || token == nil || !token.Valid {
		return nil, ErrAssertion
	}
	return claims, nil
}

// Verify accepts only a signed, unexpired assertion for the configured audience and issuer.
// The issuer must come from a compiled Registry, which normalizes it.
func (v *Verifier) Verify(ctx context.Context, spec IssuerSpec, audience, raw string) (Assertion, error) {
	if spec.TokenValidation.MaxLifetimeSeconds == nil || audience == "" || len(audience) > 2048 {
		return Assertion{}, ErrAssertion
	}
	claims, err := v.verifySigned(ctx, spec, raw, assertionTypes, jwt.WithAudience(audience))
	if err != nil {
		return Assertion{}, err
	}
	subject, subjectOK := claims["sub"].(string)
	expires, expErr := claims.GetExpirationTime()
	issued, iatErr := claims.GetIssuedAt()
	audiences, audErr := claims.GetAudience()
	if !subjectOK || subject == "" || len(subject) > 1024 || expErr != nil || iatErr != nil || audErr != nil || expires == nil || issued == nil || len(audiences) != 1 || audiences[0] != audience || !expires.After(v.now()) || !expires.After(issued.Time) || expires.Sub(issued.Time) > time.Duration(*spec.TokenValidation.MaxLifetimeSeconds)*time.Second {
		return Assertion{}, ErrAssertion
	}
	// Entra names its token ID uti.
	idClaim := "jti"
	if spec.PresetType == "microsoft_entra" {
		idClaim = "uti"
	}
	jti, jtiOK := claims[idClaim].(string)
	_, hasJTI := claims[idClaim]
	singleUse := spec.TokenValidation.ReplayProtection == ReplaySingleUse
	if ((singleUse || hasJTI) && (!jtiOK || jti == "")) || len(jti) > 512 {
		return Assertion{}, ErrAssertion
	}
	return Assertion{Subject: subject, JTI: jti, IssuedAt: issued.Time, ExpiresAt: expires.Time, Claims: map[string]any(claims), SingleUse: singleUse}, nil
}

// VerifySignature accepts a compact JWT that spec's issuer signed and that has
// not expired, carrying one of the given `typ` headers; "" allows the header
// to be absent. It applies no audience, subject or lifetime rule. Verify does
// that for identity assertions, and a caller with another token profile
// applies its own to the returned claims.
func (v *Verifier) VerifySignature(ctx context.Context, spec IssuerSpec, raw string, types []string) (map[string]any, error) {
	return v.verifySigned(ctx, spec, raw, types)
}

// keyFits reports whether key is of the kind algorithm signs with, and for
// ECDSA on its curve. The JWT library checks an ECDSA signature's width
// against the algorithm but verifies it on whatever curve the key has, so
// without this a token could name one curve's algorithm and be signed on another.
func keyFits(key any, algorithm string) bool {
	switch public := key.(type) {
	case *rsa.PublicKey:
		return strings.HasPrefix(algorithm, "RS") || strings.HasPrefix(algorithm, "PS")
	case *ecdsa.PublicKey:
		curve, known := map[string]elliptic.Curve{"ES256": elliptic.P256(), "ES384": elliptic.P384(), "ES512": elliptic.P521()}[algorithm]
		return known && public.Curve == curve
	case ed25519.PublicKey:
		return algorithm == "EdDSA"
	}
	return false
}

func matchingKey(keys []jose.JSONWebKey, kid, algorithm string) (jose.JSONWebKey, bool) {
	var matched *jose.JSONWebKey
	for i := range keys {
		key := &keys[i]
		if (kid != "" && key.KeyID != kid) || (kid == "" && len(keys) != 1) || (key.Algorithm != "" && key.Algorithm != algorithm) || !keyFits(key.Key, algorithm) {
			continue
		}
		if matched != nil {
			return jose.JSONWebKey{}, false
		}
		matched = key
	}
	if matched == nil {
		return jose.JSONWebKey{}, false
	}
	return *matched, true
}

// errNoSigningKeys is ErrInvalid for a well-formed key set in which every
// entry, if there is any, is a valid public key for a purpose other than signing.
var errNoSigningKeys = fmt.Errorf("%w: the key set holds no signing key", ErrInvalid)

func parseKeySet(raw []byte) ([]jose.JSONWebKey, error) {
	if len(raw) > maxKeyDocumentBytes || !uniqueJSONObject(raw) {
		return nil, ErrInvalid
	}
	var root map[string]json.RawMessage
	if json.Unmarshal(raw, &root) != nil {
		return nil, ErrInvalid
	}
	var entries []map[string]json.RawMessage
	if json.Unmarshal(root["keys"], &entries) != nil || entries == nil || len(entries) > 16 {
		return nil, ErrInvalid
	}
	keys := make([]jose.JSONWebKey, 0, len(entries))
	seen := make(map[string]bool, len(entries))
	// skippedUnread is set when an entry was skipped that is not plainly a
	// valid public key for another purpose. Such a set still yields its
	// signing keys, but it is not taken as a statement that there are none.
	skippedUnread := false
	for _, entry := range entries {
		// An issuer may publish its encryption keys in the same set. Entries
		// marked for another purpose are skipped; every other entry must be a
		// usable public signing key.
		signing := true
		if use, exists := entry["use"]; exists {
			var purpose string
			if json.Unmarshal(use, &purpose) != nil {
				return nil, ErrInvalid
			}
			signing = purpose == "sig"
		}
		if operations, exists := entry["key_ops"]; exists && signing {
			var ops []string
			if json.Unmarshal(operations, &ops) != nil {
				return nil, ErrInvalid
			}
			if signing = slices.Contains(ops, "verify"); signing && len(ops) != 1 {
				return nil, ErrInvalid
			}
		}
		key, valid := publicKey(entry)
		if !signing {
			skippedUnread = skippedUnread || !valid || !forAnotherPurpose(entry)
			continue
		}
		if !valid || (key.Use != "" && key.Use != "sig") || len(key.KeyID) > 256 || seen[key.KeyID] {
			return nil, ErrInvalid
		}
		seen[key.KeyID] = true
		keys = append(keys, key)
	}
	if len(keys) == 0 && skippedUnread {
		return nil, ErrInvalid
	}
	if len(keys) == 0 {
		return nil, errNoSigningKeys
	}
	return keys, nil
}

// otherOperations are the registered key operations that have nothing to do
// with signatures.
var otherOperations = []string{"encrypt", "decrypt", "wrapKey", "unwrapKey", "deriveKey", "deriveBits"}

// forAnotherPurpose reports whether a skipped entry says, in registered terms
// and without contradicting itself, that its key is for something other than
// signatures: the use "enc", or operations that are all of the other kind.
// A misspelt or unknown value says nothing.
func forAnotherPurpose(entry map[string]json.RawMessage) bool {
	use, hasUse := entry["use"]
	operations, hasOperations := entry["key_ops"]
	if hasUse {
		var purpose string
		if json.Unmarshal(use, &purpose) != nil || purpose != "enc" {
			return false
		}
	}
	if hasOperations {
		var ops []string
		if json.Unmarshal(operations, &ops) != nil || ops == nil || (len(ops) == 0 && !hasUse) ||
			slices.ContainsFunc(ops, func(op string) bool { return !slices.Contains(otherOperations, op) }) {
			return false
		}
	}
	return hasUse || hasOperations
}

// publicKey decodes one JWK entry and reports whether it is a public key of
// a supported kind with sound key material, whatever it is published for.
func publicKey(entry map[string]json.RawMessage) (jose.JSONWebKey, bool) {
	var key jose.JSONWebKey
	for _, private := range []string{"d", "p", "q", "dp", "dq", "qi", "oth", "k"} {
		if _, exists := entry[private]; exists {
			return key, false
		}
	}
	filtered := make(map[string]json.RawMessage)
	for _, field := range []string{"kty", "kid", "alg", "use", "n", "e", "crv", "x", "y"} {
		if value, exists := entry[field]; exists {
			filtered[field] = value
		}
	}
	encoded, err := json.Marshal(filtered)
	if err != nil || json.Unmarshal(encoded, &key) != nil || !key.Valid() || !key.IsPublic() {
		return key, false
	}
	switch public := key.Key.(type) {
	case *rsa.PublicKey:
		return key, public.N.Sign() > 0 && public.N.BitLen() >= 2048 && public.N.BitLen() <= 8192 && public.N.Bit(0) == 1 && public.E >= 3 && public.E%2 == 1
	case *ecdsa.PublicKey:
		return key, public.Curve.IsOnCurve(public.X, public.Y)
	case ed25519.PublicKey:
		return key, len(public) == ed25519.PublicKeySize
	}
	return key, false
}

func decodeDiscovery(raw []byte) (struct{ Issuer, JWKSURI string }, error) {
	var result struct{ Issuer, JWKSURI string }
	if !uniqueJSONObject(raw) {
		return result, ErrInvalid
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil || json.Unmarshal(fields["issuer"], &result.Issuer) != nil || json.Unmarshal(fields["jwks_uri"], &result.JWKSURI) != nil || result.Issuer == "" || result.JWKSURI == "" {
		return result, ErrInvalid
	}
	return result, nil
}

func uniqueJSONObject(raw []byte) bool {
	return uniqueJSON(raw, true)
}

func uniqueJSON(raw []byte, objectOnly bool) bool {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	budget := 16384
	var value func(int, json.Token) error
	value = func(depth int, token json.Token) error {
		budget--
		if depth > 16 || budget < 0 {
			return ErrInvalid
		}
		delimiter, container := token.(json.Delim)
		if !container {
			return nil
		}
		if delimiter != '{' && delimiter != '[' {
			return ErrInvalid
		}
		seen := make(map[string]bool)
		for decoder.More() {
			if delimiter == '{' {
				key, err := decoder.Token()
				name, ok := key.(string)
				if err != nil || !ok || seen[name] {
					return ErrInvalid
				}
				seen[name] = true
			}
			next, err := decoder.Token()
			if err != nil || value(depth+1, next) != nil {
				return ErrInvalid
			}
		}
		close, err := decoder.Token()
		if err != nil || (delimiter == '{' && close != json.Delim('}')) || (delimiter == '[' && close != json.Delim(']')) {
			return ErrInvalid
		}
		return nil
	}
	first, err := decoder.Token()
	if err != nil || (objectOnly && first != json.Delim('{')) || value(0, first) != nil {
		return false
	}
	_, err = decoder.Token()
	return err == io.EOF
}
