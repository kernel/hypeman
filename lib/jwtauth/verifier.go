// Package jwtauth verifies HMAC tokens during signing-key rotation.
package jwtauth

import (
	"context"
	"errors"
	"fmt"

	"github.com/golang-jwt/jwt/v5"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// Verifier accepts the signing key and additional verification-only keys.
// It is immutable and safe for concurrent use.
type Verifier struct {
	keys        [][]byte
	validations metric.Int64Counter
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
