// Package tokenrevocation rejects specific API tokens before their expiry.
//
// Tokens are identified by the hex SHA-256 of the raw token string, so
// revoking a token does not require it to carry a jti claim and the
// configuration never holds a usable credential. Revoking a token also
// revokes every other spelling of it that would verify.
package tokenrevocation

import (
	"crypto/elliptic"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"strings"
)

// List is a set of revoked token hashes. A nil List revokes nothing.
type List struct {
	hashes map[string]struct{}
}

// New builds a List from hex-encoded SHA-256 token hashes.
func New(hashes []string) (*List, error) {
	l := &List{hashes: make(map[string]struct{}, len(hashes))}
	for i, h := range hashes {
		h = strings.ToLower(strings.TrimSpace(h))
		if b, err := hex.DecodeString(h); err != nil || len(b) != sha256.Size {
			// Don't echo the value: a raw token pasted here by mistake would end up in logs.
			return nil, fmt.Errorf("entry %d is not a hex-encoded SHA-256 digest", i)
		}
		l.hashes[h] = struct{}{}
	}
	return l, nil
}

// Hash returns the hex-encoded SHA-256 of a token string. For an issued token
// this equals the SHA-256 of the raw string.
//
// The signature segment is canonicalized first. The JWT parser verifies the
// decoded signature bytes, not the segment text, and its non-strict base64
// decoding accepts other spellings of the same signature (unused trailing
// bits, embedded CR/LF). Without canonicalization those spellings would verify
// but hash differently, bypassing revocation.
func Hash(token string) string {
	sum := sha256.Sum256([]byte(canonicalize(token)))
	return hex.EncodeToString(sum[:])
}

// canonicalize re-encodes the signature segment, decoded the same way as
// jwt.Parser.DecodeSegment with default options. The header and payload are
// left alone: they form the signing input, so any change to them fails
// signature verification.
func canonicalize(token string) string {
	i := strings.LastIndexByte(token, '.')
	if i < 0 {
		return token
	}
	sig, err := base64.RawURLEncoding.DecodeString(token[i+1:])
	if err != nil {
		return token
	}
	return token[:i+1] + base64.RawURLEncoding.EncodeToString(sig)
}

// ecdsaTwin returns the other token that carries the same ECDSA signature.
// For a signature (r, s), (r, n-s) verifies as well, where n is the order of
// the curve, so anyone holding an ECDSA-signed token can write a second token
// string that is just as valid. A token signed any other way has no twin.
//
// The algorithm is read from the unverified header. A holder cannot use that
// to hide the twin: the header is part of the signing input, so a token with
// a changed header does not verify.
func ecdsaTwin(token string) (string, bool) {
	encodedHeader, _, _ := strings.Cut(token, ".")
	rawHeader, err := base64.RawURLEncoding.DecodeString(encodedHeader)
	// The member is looked up by its exact name, as verification does: a
	// struct field would also match "ALG".
	var header map[string]json.RawMessage
	var algorithm string
	if err != nil || json.Unmarshal(rawHeader, &header) != nil || json.Unmarshal(header["alg"], &algorithm) != nil {
		return "", false
	}
	var curve elliptic.Curve
	switch algorithm {
	case "ES256":
		curve = elliptic.P256()
	case "ES384":
		curve = elliptic.P384()
	case "ES512":
		curve = elliptic.P521()
	default:
		return "", false
	}
	i := strings.LastIndexByte(token, '.')
	signature, err := base64.RawURLEncoding.DecodeString(token[i+1:])
	size := (curve.Params().BitSize + 7) / 8
	if err != nil || len(signature) != 2*size {
		return "", false
	}
	order := curve.Params().N
	s := new(big.Int).SetBytes(signature[size:])
	if s.Sign() <= 0 || s.Cmp(order) >= 0 {
		return "", false
	}
	new(big.Int).Sub(order, s).FillBytes(signature[size:])
	return token[:i+1] + base64.RawURLEncoding.EncodeToString(signature), true
}

// IsRevoked reports whether the token has been revoked: its own hash is
// listed, or that of the other token carrying the same ECDSA signature.
func (l *List) IsRevoked(token string) bool {
	if l == nil || len(l.hashes) == 0 {
		return false
	}
	if _, ok := l.hashes[Hash(token)]; ok {
		return true
	}
	if twin, ok := ecdsaTwin(token); ok {
		_, ok = l.hashes[Hash(twin)]
		return ok
	}
	return false
}

// Len returns the number of revoked token hashes.
func (l *List) Len() int {
	if l == nil {
		return 0
	}
	return len(l.hashes)
}
