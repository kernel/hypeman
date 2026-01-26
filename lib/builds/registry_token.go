// Package builds implements registry token generation for secure builder VM authentication.
package builds

import (
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// RepoPermission defines access permissions for a specific repository.
// This enables fine-grained control where different repos can have different scopes.
type RepoPermission struct {
	// Repo is the repository path (e.g., "builds/abc123", "cache/tenant-x", "cache/global/node")
	Repo string `json:"repo"`

	// Scope is the access scope for this repo: "pull" for read-only, "push" for read+write
	Scope string `json:"scope"`
}

// RegistryTokenClaims contains the claims for a scoped registry access token.
// These tokens are issued to builder VMs to grant limited access to specific repositories.
type RegistryTokenClaims struct {
	jwt.RegisteredClaims

	// BuildID is the build job identifier for audit purposes
	BuildID string `json:"build_id"`

	// RepoAccess defines per-repository access permissions (new two-tier format)
	// If present, this takes precedence over the legacy Repositories/Scope fields
	RepoAccess []RepoPermission `json:"repo_access,omitempty"`

	// Repositories is the list of allowed repository paths (legacy format, kept for backward compat)
	// Deprecated: Use RepoAccess for new tokens
	Repositories []string `json:"repos,omitempty"`

	// Scope is the access scope (legacy format, kept for backward compat)
	// Deprecated: Use RepoAccess for new tokens
	Scope string `json:"scope,omitempty"`
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
// Deprecated: Use GenerateToken for new code that needs per-repo scopes.
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

// GenerateToken creates a short-lived token with per-repository access permissions.
// This supports the two-tier cache model where different repos can have different scopes.
// For example: pull on cache/global/*, push on cache/{tenant}
func (g *RegistryTokenGenerator) GenerateToken(buildID string, repoAccess []RepoPermission, ttl time.Duration) (string, error) {
	if buildID == "" {
		return "", fmt.Errorf("build ID is required")
	}
	if len(repoAccess) == 0 {
		return "", fmt.Errorf("at least one repository permission is required")
	}

	now := time.Now()
	claims := RegistryTokenClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   "builder-" + buildID,
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(ttl)),
			Issuer:    "hypeman",
		},
		BuildID:    buildID,
		RepoAccess: repoAccess,
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
	// Check new per-repo access format first
	if len(c.RepoAccess) > 0 {
		for _, perm := range c.RepoAccess {
			if perm.Repo == repo {
				return true
			}
		}
		return false
	}

	// Fall back to legacy format
	for _, allowed := range c.Repositories {
		if allowed == repo {
			return true
		}
	}
	return false
}

// GetRepoScope returns the scope for a specific repository.
// Returns empty string if the repository is not allowed.
func (c *RegistryTokenClaims) GetRepoScope(repo string) string {
	// Check new per-repo access format first
	if len(c.RepoAccess) > 0 {
		for _, perm := range c.RepoAccess {
			if perm.Repo == repo {
				return perm.Scope
			}
		}
		return ""
	}

	// Fall back to legacy format - all repos have the same scope
	for _, allowed := range c.Repositories {
		if allowed == repo {
			return c.Scope
		}
	}
	return ""
}

// IsPushAllowedForRepo returns true if the token grants push (write) access to the given repo.
func (c *RegistryTokenClaims) IsPushAllowedForRepo(repo string) bool {
	scope := c.GetRepoScope(repo)
	return scope == "push"
}

// IsPullAllowedForRepo returns true if the token grants pull (read) access to the given repo.
// Push scope also implicitly grants pull access.
func (c *RegistryTokenClaims) IsPullAllowedForRepo(repo string) bool {
	scope := c.GetRepoScope(repo)
	return scope == "push" || scope == "pull"
}

// IsPushAllowed returns true if the token grants push (write) access to any repo.
// Deprecated: Use IsPushAllowedForRepo for per-repo scope checking.
func (c *RegistryTokenClaims) IsPushAllowed() bool {
	// Check new per-repo access format first
	if len(c.RepoAccess) > 0 {
		for _, perm := range c.RepoAccess {
			if perm.Scope == "push" {
				return true
			}
		}
		return false
	}

	// Fall back to legacy format
	return c.Scope == "push"
}

// IsPullAllowed returns true if the token grants pull (read) access.
// Push tokens also implicitly grant pull access.
// Deprecated: Use IsPullAllowedForRepo for per-repo scope checking.
func (c *RegistryTokenClaims) IsPullAllowed() bool {
	// Check new per-repo access format first
	if len(c.RepoAccess) > 0 {
		for _, perm := range c.RepoAccess {
			if perm.Scope == "push" || perm.Scope == "pull" {
				return true
			}
		}
		return false
	}

	// Fall back to legacy format
	return c.Scope == "push" || c.Scope == "pull"
}
