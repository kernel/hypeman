package builds

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"strings"
)

// CacheKeyGenerator generates cache keys for builds with tenant isolation
type CacheKeyGenerator struct {
	registryURL string
}

// NewCacheKeyGenerator creates a new cache key generator
func NewCacheKeyGenerator(registryURL string) *CacheKeyGenerator {
	return &CacheKeyGenerator{registryURL: registryURL}
}

// CacheKey represents a validated cache key
type CacheKey struct {
	// Full reference for BuildKit --import-cache / --export-cache
	Reference string

	// Components
	TenantScope string
	Runtime     string
	LockfileHash string
}

// GenerateCacheKey generates a cache key for a build.
//
// Cache key structure:
//   {registry}/cache/{tenant_scope}/{runtime}/{lockfile_hash}
//
// This structure provides:
// - Tenant isolation: each tenant's cache is isolated by scope
// - Runtime separation: Node.js and Python caches don't mix
// - Lockfile-based keying: same lockfile = cache hit
func (g *CacheKeyGenerator) GenerateCacheKey(tenantScope, runtime string, lockfileHashes map[string]string) (*CacheKey, error) {
	if tenantScope == "" {
		return nil, fmt.Errorf("tenant scope is required for caching")
	}

	if !IsSupportedRuntime(runtime) {
		return nil, fmt.Errorf("unsupported runtime: %s", runtime)
	}

	// Normalize tenant scope (alphanumeric + hyphen only)
	normalizedScope := normalizeCacheScope(tenantScope)
	if normalizedScope == "" {
		return nil, fmt.Errorf("invalid tenant scope: %s", tenantScope)
	}

	// Compute lockfile hash from all lockfile hashes
	lockfileHash := computeCombinedHash(lockfileHashes)

	// Build the reference
	reference := fmt.Sprintf("%s/cache/%s/%s/%s",
		g.registryURL,
		normalizedScope,
		runtime,
		lockfileHash[:16], // Use first 16 chars for brevity
	)

	return &CacheKey{
		Reference:    reference,
		TenantScope:  normalizedScope,
		Runtime:      runtime,
		LockfileHash: lockfileHash,
	}, nil
}

// ValidateCacheScope validates that a cache scope is safe to use
func ValidateCacheScope(scope string) error {
	if scope == "" {
		return fmt.Errorf("cache scope is required")
	}

	normalized := normalizeCacheScope(scope)
	if normalized == "" {
		return fmt.Errorf("cache scope contains only invalid characters")
	}

	if len(normalized) < 3 {
		return fmt.Errorf("cache scope must be at least 3 characters")
	}

	if len(normalized) > 64 {
		return fmt.Errorf("cache scope must be at most 64 characters")
	}

	return nil
}

// ImportCacheArg returns the BuildKit --import-cache argument
func (k *CacheKey) ImportCacheArg() string {
	return fmt.Sprintf("type=registry,ref=%s", k.Reference)
}

// ExportCacheArg returns the BuildKit --export-cache argument
func (k *CacheKey) ExportCacheArg() string {
	return fmt.Sprintf("type=registry,ref=%s,mode=max", k.Reference)
}

// normalizeCacheScope normalizes a cache scope to only contain safe characters
// for use in registry paths (alphanumeric and hyphens)
func normalizeCacheScope(scope string) string {
	// Convert to lowercase and replace unsafe characters
	scope = strings.ToLower(scope)

	// Keep only alphanumeric and hyphens
	re := regexp.MustCompile(`[^a-z0-9-]`)
	normalized := re.ReplaceAllString(scope, "-")

	// Remove consecutive hyphens
	re = regexp.MustCompile(`-+`)
	normalized = re.ReplaceAllString(normalized, "-")

	// Trim leading/trailing hyphens
	normalized = strings.Trim(normalized, "-")

	return normalized
}

// computeCombinedHash computes a combined hash from multiple lockfile hashes
func computeCombinedHash(lockfileHashes map[string]string) string {
	if len(lockfileHashes) == 0 {
		return "empty"
	}

	// Sort keys for determinism
	h := sha256.New()
	for _, name := range sortedKeys(lockfileHashes) {
		h.Write([]byte(name))
		h.Write([]byte(":"))
		h.Write([]byte(lockfileHashes[name]))
		h.Write([]byte("\n"))
	}

	return hex.EncodeToString(h.Sum(nil))
}

// sortedKeys returns the keys of a map in sorted order
func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	// Simple bubble sort for small maps (lockfiles are typically 1-3)
	for i := 0; i < len(keys)-1; i++ {
		for j := i + 1; j < len(keys); j++ {
			if keys[i] > keys[j] {
				keys[i], keys[j] = keys[j], keys[i]
			}
		}
	}
	return keys
}

// GetCacheKeyFromConfig extracts cache configuration for the builder agent
func GetCacheKeyFromConfig(registryURL, cacheScope, runtime string, lockfileHashes map[string]string) (importArg, exportArg string, err error) {
	if cacheScope == "" {
		return "", "", nil // Caching disabled
	}

	gen := NewCacheKeyGenerator(registryURL)
	key, err := gen.GenerateCacheKey(cacheScope, runtime, lockfileHashes)
	if err != nil {
		return "", "", err
	}

	return key.ImportCacheArg(), key.ExportCacheArg(), nil
}

