// Package accesstoken verifies the access tokens an OAuth authorization server
// issues for this server, in the JWT format of RFC 9068.
package accesstoken

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/kernel/hypeman/lib/oidcfederation"
)

const (
	maxAudiences = 16
	maxServers   = 8
)

var (
	ErrInvalidConfig = errors.New("invalid oauth configuration")
	// ErrToken is returned for every token that is not accepted, whatever the reason.
	ErrToken = errors.New("invalid access token")
	// ErrUnavailable means no key of the token's authorization server could be obtained.
	ErrUnavailable = errors.New("authorization server keys unavailable")

	// tokenTypes are the `typ` headers RFC 9068 gives an access token. A
	// token without one is not an access token.
	tokenTypes = []string{"at+jwt", "application/at+jwt"}
)

// AuthorizationServer is one issuer of access tokens this server accepts.
type AuthorizationServer struct {
	// Issuer is the server's issuer URL, compared exactly with a token's `iss`.
	Issuer string `koanf:"issuer"`
	// JWKS says where the server's public keys come from. The default,
	// discovery, reads the key set the server's RFC 8414 metadata names.
	JWKS oidcfederation.KeySource `koanf:"jwks"`
}

// Config is the `oauth` block of the server configuration.
type Config struct {
	// Audiences are the names this server answers to. A token is accepted
	// only if its `aud` holds at least one of them.
	Audiences            []string              `koanf:"audiences"`
	AuthorizationServers []AuthorizationServer `koanf:"authorization_servers"`
}

// Claims are the verified claims of an access token.
type Claims struct {
	Issuer   string
	Subject  string
	ClientID string
	// ID is the token's `jti`.
	ID        string
	Audience  []string
	IssuedAt  time.Time
	ExpiresAt time.Time
	// Permissions are the entries of the token's `scope`. A token without a
	// scope has none: it never has the full access of a token signed with the
	// server's own secret that lacks a permissions claim.
	Permissions []string
}

// Verifier checks access tokens against the configured authorization servers.
// It is safe for concurrent use.
type Verifier struct {
	audiences []string
	servers   map[string]oidcfederation.IssuerSpec
	keys      *oidcfederation.Verifier
}

// New validates the configuration. A nil store keeps fetched keys in memory
// only, and a nil log leaves a failing key fetch unreported.
func New(cfg Config, store oidcfederation.KeyStore, log *slog.Logger) (*Verifier, error) {
	if len(cfg.AuthorizationServers) == 0 {
		if len(cfg.Audiences) != 0 {
			return nil, fmt.Errorf("%w: oauth.audiences is set without any oauth.authorization_servers", ErrInvalidConfig)
		}
		return &Verifier{}, nil
	}
	if len(cfg.Audiences) == 0 || len(cfg.Audiences) > maxAudiences {
		return nil, fmt.Errorf("%w: oauth.audiences needs between 1 and %d entries", ErrInvalidConfig, maxAudiences)
	}
	for i, audience := range cfg.Audiences {
		if audience == "" || len(audience) > 2048 || slices.Contains(cfg.Audiences[:i], audience) {
			return nil, fmt.Errorf("%w: oauth.audiences[%d] is empty, too long or repeated", ErrInvalidConfig, i)
		}
	}
	if len(cfg.AuthorizationServers) > maxServers {
		return nil, fmt.Errorf("%w: at most %d oauth.authorization_servers are supported", ErrInvalidConfig, maxServers)
	}
	servers := make(map[string]oidcfederation.IssuerSpec, len(cfg.AuthorizationServers))
	for i, server := range cfg.AuthorizationServers {
		spec, err := oidcfederation.AuthorizationServer(server.Issuer, server.JWKS)
		if err != nil {
			return nil, fmt.Errorf("%w: oauth.authorization_servers[%d]: %w", ErrInvalidConfig, i, err)
		}
		if _, exists := servers[spec.IssuerURL]; exists {
			return nil, fmt.Errorf("%w: oauth.authorization_servers[%d]: issuer is configured twice", ErrInvalidConfig, i)
		}
		servers[spec.IssuerURL] = spec
	}
	return &Verifier{audiences: slices.Clone(cfg.Audiences), servers: servers, keys: oidcfederation.NewVerifierWithKeyStore(store, log)}, nil
}

// Enabled reports whether any authorization server is configured.
func (v *Verifier) Enabled() bool {
	return v != nil && len(v.servers) > 0
}

// Verify accepts a signed, unexpired access token that a configured
// authorization server issued for one of this server's audiences.
func (v *Verifier) Verify(ctx context.Context, raw string) (Claims, error) {
	if !v.Enabled() {
		return Claims{}, ErrToken
	}
	// The issuer claim only selects whose keys to check the signature with.
	server, known := v.servers[unverifiedIssuer(raw)]
	if !known {
		return Claims{}, ErrToken
	}
	verified, err := v.keys.VerifySignature(ctx, server, raw, tokenTypes)
	if errors.Is(err, oidcfederation.ErrUnavailable) {
		return Claims{}, fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	if err != nil {
		return Claims{}, ErrToken
	}
	// RFC 9068 requires every claim read here except the scope.
	claims := jwt.MapClaims(verified)
	subject, _ := claims["sub"].(string)
	client, _ := claims["client_id"].(string)
	id, _ := claims["jti"].(string)
	expires, expErr := claims.GetExpirationTime()
	issued, iatErr := claims.GetIssuedAt()
	audiences, audErr := claims.GetAudience()
	if subject == "" || len(subject) > 1024 || client == "" || len(client) > 1024 || id == "" || len(id) > 512 ||
		expErr != nil || iatErr != nil || audErr != nil || expires == nil || issued == nil ||
		!slices.ContainsFunc(audiences, func(audience string) bool { return slices.Contains(v.audiences, audience) }) {
		return Claims{}, ErrToken
	}
	out := Claims{Issuer: server.IssuerURL, Subject: subject, ClientID: client, ID: id, Audience: audiences, IssuedAt: issued.Time, ExpiresAt: expires.Time, Permissions: []string{}}
	if scope, exists := claims["scope"]; exists {
		text, ok := scope.(string)
		if !ok {
			return Claims{}, ErrToken
		}
		out.Permissions = strings.Fields(text)
	}
	return out, nil
}

// Typed reports whether raw declares itself an access token in its `typ`
// header, whatever else is wrong with it. Nothing about the token is verified.
func Typed(raw string) bool {
	encoded, _, found := strings.Cut(raw, ".")
	decoded, err := base64.RawURLEncoding.DecodeString(encoded)
	if !found || err != nil {
		return false
	}
	tokenType, _ := member(decoded, "typ")
	return slices.ContainsFunc(tokenTypes, func(allowed string) bool { return strings.EqualFold(allowed, tokenType) })
}

// unverifiedIssuer reads the `iss` claim of a compact JWT of acceptable size.
// It is empty for anything else.
func unverifiedIssuer(raw string) string {
	if len(raw) > oidcfederation.MaxAssertionBytes {
		return ""
	}
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return ""
	}
	decoded, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return ""
	}
	issuer, _ := member(decoded, "iss")
	return issuer
}

// member reads a text member of a JSON object by its exact name, as
// verification does: a struct field would also match another capitalization.
func member(object []byte, name string) (string, bool) {
	var members map[string]json.RawMessage
	var value string
	if json.Unmarshal(object, &members) != nil || json.Unmarshal(members[name], &value) != nil {
		return "", false
	}
	return value, true
}
