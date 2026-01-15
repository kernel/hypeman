// Package builds implements registry token generation for secure builder VM authentication.
package builds

import (
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// RegistryTokenClaims contains the claims for a scoped registry access token.
// These tokens are issued to builder VMs to grant limited push access to specific repositories.
type RegistryTokenClaims struct {
	jwt.RegisteredClaims

	// BuildID is the build job identifier for audit purposes
	BuildID string `json:"build_id"`

	// Repositories is the list of allowed repository paths (e.g., ["builds/abc123", "cache/tenant-x"])
	Repositories []string `json:"repos"`

	// Scope is the access scope: "push" for write access, "pull" for read-only
	Scope string `json:"scope"`
}

// RegistryTokenGenerator creates scoped registry access tokens
type RegistryTokenGenerator struct {
	secret []byte
}

// NewRegistryTokenGenerator creates a new token generator with the given secret
func NewRegistryTokenGenerator(secret string) *RegistryTokenGenerator {
	return &RegistryTokenGenerator{
		secret: []byte(secret),
	}
}

// GeneratePushToken creates a short-lived token granting push access to specific repositories.
// The token expires after the specified duration (typically matching the build timeout).
func (g *RegistryTokenGenerator) GeneratePushToken(buildID string, repos []string, ttl time.Duration) (string, error) {
	if buildID == "" {
		return "", fmt.Errorf("build ID is required")
	}
	if len(repos) == 0 {
		return "", fmt.Errorf("at least one repository is required")
	}

	now := time.Now()
	claims := RegistryTokenClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   "builder-" + buildID,
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(ttl)),
			Issuer:    "hypeman",
		},
		BuildID:      buildID,
		Repositories: repos,
		Scope:        "push",
	}

	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	return token.SignedString(g.secret)
}

// ValidateToken parses and validates a registry token, returning the claims if valid.
func (g *RegistryTokenGenerator) ValidateToken(tokenString string) (*RegistryTokenClaims, error) {
	token, err := jwt.ParseWithClaims(tokenString, &RegistryTokenClaims{}, func(token *jwt.Token) (interface{}, error) {
		// Validate signing method
		if _, ok := token.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method: %v", token.Header["alg"])
		}
		return g.secret, nil
	})

	if err != nil {
		return nil, fmt.Errorf("parse token: %w", err)
	}

	claims, ok := token.Claims.(*RegistryTokenClaims)
	if !ok || !token.Valid {
		return nil, fmt.Errorf("invalid token")
	}

	return claims, nil
}

// IsRepositoryAllowed checks if the given repository path is allowed by the token claims.
func (c *RegistryTokenClaims) IsRepositoryAllowed(repo string) bool {
	for _, allowed := range c.Repositories {
		if allowed == repo {
			return true
		}
	}
	return false
}

// IsPushAllowed returns true if the token grants push (write) access.
func (c *RegistryTokenClaims) IsPushAllowed() bool {
	return c.Scope == "push"
}

// IsPullAllowed returns true if the token grants pull (read) access.
// Push tokens also implicitly grant pull access.
func (c *RegistryTokenClaims) IsPullAllowed() bool {
	return c.Scope == "push" || c.Scope == "pull"
}
