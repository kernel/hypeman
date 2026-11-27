// Package jwtauth verifies API tokens: HMAC tokens signed with the server's
// own secret, during signing-key rotation too, and access tokens issued by a
// configured authorization server.
package jwtauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/golang-jwt/jwt/v5"
	"github.com/kernel/hypeman/lib/accesstoken"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// Verifier accepts the signing key and additional verification-only keys.
// It is immutable and safe for concurrent use.
type Verifier struct {
	keys         [][]byte
	validations  metric.Int64Counter
	accessTokens *accesstoken.Verifier
}

// WithAccessTokens returns a Verifier that also accepts the access tokens
// that verifier does.
func (v *Verifier) WithAccessTokens(verifier *accesstoken.Verifier) *Verifier {
	out := *v
	out.accessTokens = verifier
	return &out
}

func NewVerifier(signingSecret string, verificationSecrets []string, meter metric.Meter) (*Verifier, error) {
	if signingSecret == "" {
		return nil, errors.New("JWT_SECRET must be set")
	}
	keys := [][]byte{[]byte(signingSecret)}
	for i, secret := range verificationSecrets {
		if secret == "" {
			return nil, fmt.Errorf("jwt_verification_secrets entry %d must not be empty", i)
		}
		keys = append(keys, []byte(secret))
	}
	validations, err := meter.Int64Counter("hypeman_jwt_validations_total",
		metric.WithDescription("Successful JWT signature and claims validations by key role; not authorization decisions"))
	if err != nil {
		return nil, err
	}
	return &Verifier{keys: keys, validations: validations}, nil
}

// ParseWithClaims preserves JWT claim validation and only tries another key on
// a signature mismatch. Callers still enforce revocation and authorization.
func (v *Verifier) ParseWithClaims(ctx context.Context, tokenString string, claims jwt.Claims) (*jwt.Token, error) {
	// A token that declares itself an access token is never checked against
	// the HMAC keys, and no other token is checked against an authorization
	// server's keys.
	if v.IsAccessToken(tokenString) {
		return v.parseAccessToken(ctx, tokenString, claims)
	}
	if len(v.keys) == 0 {
		return nil, errors.New("JWT secret not configured")
	}
	var err error
	for i, key := range v.keys {
		var token *jwt.Token
		token, err = jwt.ParseWithClaims(tokenString, claims, func(token *jwt.Token) (interface{}, error) {
			if _, ok := token.Method.(*jwt.SigningMethodHMAC); !ok {
				return nil, fmt.Errorf("unexpected signing method: %v", token.Header["alg"])
			}
			return key, nil
		})
		if err == nil {
			role := "primary"
			if i > 0 {
				role = "fallback"
			}
			v.validations.Add(ctx, 1, metric.WithAttributes(attribute.String("key_role", role)))
			return token, nil
		}
		if !errors.Is(err, jwt.ErrTokenSignatureInvalid) {
			return nil, err
		}
	}
	return nil, err
}

// IsAccessToken reports whether ParseWithClaims checks tokenString against an
// authorization server's keys rather than the HMAC keys.
func (v *Verifier) IsAccessToken(tokenString string) bool {
	return v.accessTokens.Enabled() && accesstoken.Typed(tokenString)
}

// parseAccessToken verifies an authorization server's access token and hands
// its claims to the caller in the shape of an HMAC token's: the scope becomes
// the permissions list. The list is always present, so such a token is never
// read as a token without one, which has full access.
func (v *Verifier) parseAccessToken(ctx context.Context, tokenString string, claims jwt.Claims) (*jwt.Token, error) {
	verified, err := v.accessTokens.Verify(ctx, tokenString)
	if err != nil {
		return nil, err
	}
	fields := map[string]any{
		"iss":         verified.Issuer,
		"sub":         verified.Subject,
		"aud":         verified.Audience,
		"iat":         verified.IssuedAt.Unix(),
		"exp":         verified.ExpiresAt.Unix(),
		"jti":         verified.ID,
		"permissions": verified.Permissions,
	}
	encoded, err := json.Marshal(fields)
	if err != nil {
		return nil, err
	}
	// MapClaims is passed by value; decoding through its address fills the caller's map.
	if mapped, ok := claims.(jwt.MapClaims); ok {
		err = json.Unmarshal(encoded, &mapped)
	} else {
		err = json.Unmarshal(encoded, claims)
	}
	if err != nil {
		return nil, err
	}
	v.validations.Add(ctx, 1, metric.WithAttributes(attribute.String("key_role", "authorization_server")))
	return &jwt.Token{Raw: tokenString, Claims: claims, Valid: true}, nil
}
