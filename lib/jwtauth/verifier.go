// Package jwtauth verifies HMAC tokens during signing-key rotation.
package jwtauth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/golang-jwt/jwt/v5"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// ErrFederationInactive is returned for a correctly signed token that was
// issued for an OIDC federation rule which is no longer configured as it was.
var ErrFederationInactive = errors.New("federated token is not valid for the current configuration")

// FederationChecker reports whether the federation rule a token was issued for
// is still configured, trusts the same issuer, and is unchanged since then.
type FederationChecker interface {
	Current(issuer, rule, revision string) bool
}

// Verifier accepts the signing key and additional verification-only keys.
// It is immutable and safe for concurrent use.
type Verifier struct {
	keys        [][]byte
	validations metric.Int64Counter
	federation  FederationChecker
}

// WithFederation returns a Verifier that accepts federated tokens the checker
// reports as current. Without one, every federated token is rejected.
func (v *Verifier) WithFederation(checker FederationChecker) *Verifier {
	out := *v
	out.federation = checker
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
			if err := v.checkFederation(tokenString); err != nil {
				return nil, err
			}
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

// checkFederation applies to every token carrying a `fed` claim, whatever
// claims type the caller parses into: it reads the verified payload itself.
// Such a token is bound to one federation rule and must carry that rule's
// subject and an explicit permissions list, so it can never be read as a
// legacy full-access token.
func (v *Verifier) checkFederation(tokenString string) error {
	parts := strings.Split(tokenString, ".")
	if len(parts) != 3 {
		return ErrFederationInactive
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return ErrFederationInactive
	}
	var claims map[string]json.RawMessage
	if err := json.Unmarshal(payload, &claims); err != nil {
		return ErrFederationInactive
	}
	federation, federated := claims["fed"]
	if !federated {
		return nil
	}
	var lineage struct {
		Issuer   string `json:"issuer"`
		Rule     string `json:"rule"`
		Revision string `json:"rev"`
	}
	var subject string
	var permissions []string
	if json.Unmarshal(federation, &lineage) != nil || lineage.Rule == "" ||
		json.Unmarshal(claims["sub"], &subject) != nil || subject != "federation:"+lineage.Rule ||
		json.Unmarshal(claims["permissions"], &permissions) != nil || permissions == nil ||
		v.federation == nil || !v.federation.Current(lineage.Issuer, lineage.Rule, lineage.Revision) {
		return ErrFederationInactive
	}
	return nil
}
