package authserver

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"

	jose "github.com/go-jose/go-jose/v4"
)

// ErrKey is returned for a key that cannot sign or vouch for access tokens.
var ErrKey = errors.New("invalid access token key")

// SigningKey signs access tokens with ES256. ID is the `kid` its tokens carry.
type SigningKey struct {
	private *ecdsa.PrivateKey
	ID      string
}

// ParseSigningKey reads a PEM-encoded P-256 private key, in PKCS #8 or SEC 1 form.
func ParseSigningKey(encoded []byte) (*SigningKey, error) {
	block, _ := pem.Decode(encoded)
	if block == nil {
		return nil, fmt.Errorf("%w: the signing key is not PEM", ErrKey)
	}
	var private *ecdsa.PrivateKey
	switch block.Type {
	case "PRIVATE KEY":
		parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("%w: the signing key does not parse", ErrKey)
		}
		private, _ = parsed.(*ecdsa.PrivateKey)
	case "EC PRIVATE KEY":
		parsed, err := x509.ParseECPrivateKey(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("%w: the signing key does not parse", ErrKey)
		}
		private = parsed
	}
	if private == nil || private.Curve != elliptic.P256() {
		return nil, fmt.Errorf("%w: the signing key must be an ECDSA P-256 private key", ErrKey)
	}
	id, err := keyID(&private.PublicKey)
	if err != nil {
		return nil, err
	}
	return &SigningKey{private: private, ID: id}, nil
}

// ParsePublicKey reads a PEM-encoded P-256 public key, as published beside
// the signing key while tokens it signed are still in use.
func ParsePublicKey(encoded []byte) (*ecdsa.PublicKey, error) {
	block, _ := pem.Decode(encoded)
	if block == nil || block.Type != "PUBLIC KEY" {
		return nil, fmt.Errorf("%w: a published key must be a PEM public key", ErrKey)
	}
	parsed, err := x509.ParsePKIXPublicKey(block.Bytes)
	public, _ := parsed.(*ecdsa.PublicKey)
	if err != nil || public == nil || public.Curve != elliptic.P256() {
		return nil, fmt.Errorf("%w: a published key must be an ECDSA P-256 public key", ErrKey)
	}
	return public, nil
}

// keyID is the RFC 7638 thumbprint of a public key, so a key has the same ID
// wherever it is configured.
func keyID(public *ecdsa.PublicKey) (string, error) {
	thumbprint, err := (&jose.JSONWebKey{Key: public}).Thumbprint(crypto.SHA256)
	if err != nil {
		return "", fmt.Errorf("%w: %w", ErrKey, err)
	}
	return base64.RawURLEncoding.EncodeToString(thumbprint), nil
}

// maxKeys is how many keys a Hypeman server accepts in one key set.
const maxKeys = 16

// keySet is the JWKS document: the signing key first, then the published
// keys. Each key is listed once, because a Hypeman server refuses a key set
// in which a key ID repeats.
func keySet(signing *SigningKey, published []*ecdsa.PublicKey) ([]byte, error) {
	set := jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &signing.private.PublicKey, KeyID: signing.ID, Algorithm: "ES256", Use: "sig"}}}
	listed := map[string]bool{signing.ID: true}
	for _, public := range published {
		id, err := keyID(public)
		if err != nil {
			return nil, err
		}
		if listed[id] {
			continue
		}
		listed[id] = true
		set.Keys = append(set.Keys, jose.JSONWebKey{Key: public, KeyID: id, Algorithm: "ES256", Use: "sig"})
	}
	if len(set.Keys) > maxKeys {
		return nil, fmt.Errorf("%w: at most %d keys can be published", ErrKey, maxKeys)
	}
	return json.Marshal(set)
}
