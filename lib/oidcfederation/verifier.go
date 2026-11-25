package oidcfederation

import (
	"bytes"
	"cmp"
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
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
}

type keyLoad struct {
	done chan struct{}
	keys []jose.JSONWebKey
	err  error
}

// Verifier bounds issuer key fetching and validates the supported OIDC assertion profile.
type Verifier struct {
	mu      sync.Mutex
	cache   map[string]cachedKeys
	loading map[string]*keyLoad
	fetch   func(context.Context, IssuerSpec) ([]byte, error)
	now     func() time.Time
}

// NewVerifier uses HTTPS public-address-only key retrieval and an in-memory bounded cache.
func NewVerifier() *Verifier {
	return &Verifier{cache: make(map[string]cachedKeys), loading: make(map[string]*keyLoad), fetch: fetchIssuerKeys, now: time.Now}
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
	if now.Before(entry.expiresAt) && !force {
		v.mu.Unlock()
		return entry.keys, nil
	}
	load, loading := v.loading[key]
	if !loading {
		if now.Before(entry.failureUntil) {
			v.mu.Unlock()
			return nil, cmp.Or(entry.failure, ErrUnavailable)
		}
		if force && now.Before(entry.refreshAfter) {
			v.mu.Unlock()
			return entry.keys, nil
		}
		if len(v.loading) >= maxKeyLoads {
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
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-load.done:
		return load.keys, load.err
	}
}

func (v *Verifier) loadKeys(key string, issuer IssuerSpec, load *keyLoad, startedAt time.Time) {
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	raw, err := v.fetch(ctx, issuer)
	var keys []jose.JSONWebKey
	if err == nil {
		if keys, err = parseKeySet(raw); err != nil {
			err = &KeyProblem{"the key set is not valid or holds an unsupported key"}
		}
	}
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

// Verify accepts only a signed, unexpired assertion for the configured audience and issuer.
// The issuer must come from a compiled Registry, which normalizes it.
func (v *Verifier) Verify(ctx context.Context, spec IssuerSpec, audience, raw string) (Assertion, error) {
	if spec.Name == "" || spec.TokenValidation.MaxLifetimeSeconds == nil || audience == "" || len(audience) > 2048 || len(raw) > MaxAssertionBytes {
		return Assertion{}, ErrAssertion
	}
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return Assertion{}, ErrAssertion
	}
	header, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil || !uniqueJSONObject(header) {
		return Assertion{}, ErrAssertion
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil || !uniqueJSONObject(payload) {
		return Assertion{}, ErrAssertion
	}
	var protected struct{ Algorithm, KeyID, Type string }
	var fields map[string]json.RawMessage
	if json.Unmarshal(header, &fields) != nil {
		return Assertion{}, ErrAssertion
	}
	if _, critical := fields["crit"]; critical {
		return Assertion{}, ErrAssertion
	}
	for name, destination := range map[string]*string{"alg": &protected.Algorithm, "kid": &protected.KeyID, "typ": &protected.Type} {
		if value, exists := fields[name]; exists {
			value = bytes.TrimSpace(value)
			if len(value) == 0 || value[0] != '"' || json.Unmarshal(value, destination) != nil {
				return Assertion{}, ErrAssertion
			}
		}
	}
	if len(protected.KeyID) > 256 || (protected.Type != "" && !strings.EqualFold(protected.Type, "JWT") && !strings.EqualFold(protected.Type, "application/jwt")) {
		return Assertion{}, ErrAssertion
	}
	var unverified map[string]json.RawMessage
	if json.Unmarshal(payload, &unverified) != nil {
		return Assertion{}, ErrAssertion
	}
	var claimedIssuer string
	if json.Unmarshal(unverified["iss"], &claimedIssuer) != nil || claimedIssuer != spec.IssuerURL {
		return Assertion{}, ErrAssertion
	}
	algorithms := []string{"RS256", "RS384", "RS512", "PS256", "PS384", "PS512", "ES256", "ES384", "ES512", "EdDSA"}
	if spec.IssuerURL == GitHubIssuer || spec.IssuerURL == GoogleIssuer || spec.PresetType == "microsoft_entra" {
		algorithms = []string{"RS256"}
	}
	if !slices.Contains(algorithms, protected.Algorithm) {
		return Assertion{}, ErrAssertion
	}
	keys, err := v.keySet(ctx, spec, false)
	if err != nil {
		return Assertion{}, err
	}
	key, ok := matchingKey(keys, protected.KeyID, protected.Algorithm)
	if !ok {
		keys, err = v.keySet(ctx, spec, true)
		if err != nil {
			return Assertion{}, err
		}
		key, ok = matchingKey(keys, protected.KeyID, protected.Algorithm)
		if !ok {
			return Assertion{}, ErrAssertion
		}
	}
	claims := jwt.MapClaims{}
	token, err := jwt.ParseWithClaims(raw, claims, func(*jwt.Token) (any, error) { return key.Key, nil }, jwt.WithValidMethods(algorithms), jwt.WithIssuer(spec.IssuerURL), jwt.WithAudience(audience), jwt.WithExpirationRequired(), jwt.WithIssuedAt(), jwt.WithLeeway(clockLeeway), jwt.WithTimeFunc(v.now), jwt.WithJSONNumber())
	if err != nil || token == nil || !token.Valid {
		return Assertion{}, ErrAssertion
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

func matchingKey(keys []jose.JSONWebKey, kid, algorithm string) (jose.JSONWebKey, bool) {
	var matched *jose.JSONWebKey
	for i := range keys {
		key := &keys[i]
		if (kid != "" && key.KeyID != kid) || (kid == "" && len(keys) != 1) || (key.Algorithm != "" && key.Algorithm != algorithm) {
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

func parseKeySet(raw []byte) ([]jose.JSONWebKey, error) {
	if len(raw) > maxKeyDocumentBytes || !uniqueJSONObject(raw) {
		return nil, ErrInvalid
	}
	var root map[string]json.RawMessage
	if json.Unmarshal(raw, &root) != nil {
		return nil, ErrInvalid
	}
	var entries []map[string]json.RawMessage
	if json.Unmarshal(root["keys"], &entries) != nil || len(entries) == 0 || len(entries) > 16 {
		return nil, ErrInvalid
	}
	keys := make([]jose.JSONWebKey, 0, len(entries))
	seen := make(map[string]bool, len(entries))
	for _, entry := range entries {
		// An issuer may publish its encryption keys in the same set. Entries
		// marked for another purpose are skipped; every other entry must be a
		// usable public signing key.
		if use, exists := entry["use"]; exists {
			var purpose string
			if json.Unmarshal(use, &purpose) != nil {
				return nil, ErrInvalid
			}
			if purpose != "sig" {
				continue
			}
		}
		if operations, exists := entry["key_ops"]; exists {
			var ops []string
			if json.Unmarshal(operations, &ops) != nil {
				return nil, ErrInvalid
			}
			if !slices.Contains(ops, "verify") {
				continue
			}
			if len(ops) != 1 {
				return nil, ErrInvalid
			}
		}
		for _, private := range []string{"d", "p", "q", "dp", "dq", "qi", "oth", "k"} {
			if _, exists := entry[private]; exists {
				return nil, ErrInvalid
			}
		}
		filtered := make(map[string]json.RawMessage)
		for _, field := range []string{"kty", "kid", "alg", "use", "n", "e", "crv", "x", "y"} {
			if value, exists := entry[field]; exists {
				filtered[field] = value
			}
		}
		encoded, err := json.Marshal(filtered)
		if err != nil {
			return nil, ErrInvalid
		}
		var key jose.JSONWebKey
		if json.Unmarshal(encoded, &key) != nil || !key.Valid() || !key.IsPublic() || (key.Use != "" && key.Use != "sig") || len(key.KeyID) > 256 || seen[key.KeyID] {
			return nil, ErrInvalid
		}
		switch public := key.Key.(type) {
		case *rsa.PublicKey:
			if public.N.Sign() <= 0 || public.N.BitLen() < 2048 || public.N.BitLen() > 8192 || public.N.Bit(0) == 0 || public.E < 3 || public.E%2 == 0 {
				return nil, ErrInvalid
			}
		case *ecdsa.PublicKey:
			if !public.Curve.IsOnCurve(public.X, public.Y) {
				return nil, ErrInvalid
			}
		case ed25519.PublicKey:
			if len(public) != ed25519.PublicKeySize {
				return nil, ErrInvalid
			}
		default:
			return nil, ErrInvalid
		}
		seen[key.KeyID] = true
		keys = append(keys, key)
	}
	if len(keys) == 0 {
		return nil, ErrInvalid
	}
	return keys, nil
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
