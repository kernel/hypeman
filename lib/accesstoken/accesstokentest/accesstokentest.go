// Package accesstokentest issues access tokens for tests of the code that
// accepts them.
package accesstokentest

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"maps"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/golang-jwt/jwt/v5"
	"github.com/kernel/hypeman/lib/accesstoken"
	"github.com/stretchr/testify/require"
)

const (
	Issuer   = "https://auth.example.test"
	Audience = "https://hypeman.example.test"
	keyID    = "key-a"
)

// Server stands in for an authorization server whose public key is
// configured inline, so nothing is fetched.
type Server struct {
	Key *ecdsa.PrivateKey
	// Config trusts the server for Audience.
	Config accesstoken.Config
}

// NewServer generates a signing key and the configuration that trusts it.
func NewServer(t *testing.T) *Server {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	raw, err := json.Marshal(jose.JSONWebKey{Key: &key.PublicKey, KeyID: keyID, Algorithm: "ES256", Use: "sig"})
	require.NoError(t, err)
	var public map[string]any
	require.NoError(t, json.Unmarshal(raw, &public))
	server := &Server{Key: key}
	server.Config.Audiences = []string{Audience}
	server.Config.AuthorizationServers = []accesstoken.AuthorizationServer{{Issuer: Issuer}}
	server.Config.AuthorizationServers[0].JWKS.Type = "inline"
	server.Config.AuthorizationServers[0].JWKS.Keys = []map[string]any{public}
	return server
}

// Verifier returns a verifier that trusts the server.
func (s *Server) Verifier(t *testing.T) *accesstoken.Verifier {
	t.Helper()
	verifier, err := accesstoken.New(s.Config, nil, nil)
	require.NoError(t, err)
	return verifier
}

// Claims are those of a token for subject, valid for an hour around now, with
// a token ID of its own.
func Claims(subject, scope string) jwt.MapClaims {
	now := time.Now()
	var id [16]byte
	_, _ = rand.Read(id[:])
	return jwt.MapClaims{"iss": Issuer, "sub": subject, "client_id": "test-client", "aud": Audience, "iat": now.Add(-time.Minute).Unix(), "exp": now.Add(time.Hour).Unix(), "jti": hex.EncodeToString(id[:]), "scope": scope}
}

// Token signs claims as an access token. Entries of header replace the
// defaults, among them the `typ` that marks an access token.
func (s *Server) Token(t *testing.T, claims jwt.MapClaims, header map[string]any) string {
	t.Helper()
	token := jwt.NewWithClaims(jwt.SigningMethodES256, claims)
	token.Header["kid"] = keyID
	token.Header["typ"] = "at+jwt"
	maps.Copy(token.Header, header)
	raw, err := token.SignedString(s.Key)
	require.NoError(t, err)
	return raw
}
