package builds

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCacheKeyGenerator_GenerateCacheKey(t *testing.T) {
	gen := NewCacheKeyGenerator("localhost:8080")

	tests := []struct {
		name           string
		tenantScope    string
		runtime        string
		lockfileHashes map[string]string
		wantErr        bool
		wantPrefix     string
	}{
		{
			name:        "valid nodejs build",
			tenantScope: "tenant-abc",
			runtime:     "nodejs",
			lockfileHashes: map[string]string{
				"package-lock.json": "abc123",
			},
			wantPrefix: "localhost:8080/cache/tenant-abc/nodejs/",
		},
		{
			name:        "valid python build",
			tenantScope: "my-team",
			runtime:     "python",
			lockfileHashes: map[string]string{
				"requirements.txt": "def456",
			},
			wantPrefix: "localhost:8080/cache/my-team/python/",
		},
		{
			name:        "empty tenant scope",
			tenantScope: "",
			runtime:     "nodejs",
			wantErr:     true,
		},
		{
			name:        "any runtime is accepted",
			tenantScope: "tenant",
			runtime:     "ruby",
			lockfileHashes: map[string]string{
				"Gemfile.lock": "abc123",
			},
			wantPrefix: "localhost:8080/cache/tenant/ruby/",
		},
		{
			name:        "scope with special chars",
			tenantScope: "My Team!@#$%",
			runtime:     "nodejs",
			lockfileHashes: map[string]string{
				"package-lock.json": "abc",
			},
			wantPrefix: "localhost:8080/cache/my-team/nodejs/",
		},
		{
			name:           "empty lockfileHashes does not panic",
			tenantScope:    "tenant-abc",
			runtime:        "nodejs",
			lockfileHashes: map[string]string{},
			wantPrefix:     "localhost:8080/cache/tenant-abc/nodejs/",
		},
		{
			name:           "nil lockfileHashes does not panic",
			tenantScope:    "tenant-abc",
			runtime:        "python",
			lockfileHashes: nil,
			wantPrefix:     "localhost:8080/cache/tenant-abc/python/",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			key, err := gen.GenerateCacheKey(tt.tenantScope, tt.runtime, tt.lockfileHashes)

			if tt.wantErr {
				require.Error(t, err)
				return
			}

			require.NoError(t, err)
			assert.Contains(t, key.Reference, tt.wantPrefix)
		})
	}
}

func TestCacheKey_Args(t *testing.T) {
	key := &CacheKey{
		Reference:    "localhost:8080/cache/tenant/nodejs/abc123",
		TenantScope:  "tenant",
		Runtime:      "nodejs",
		LockfileHash: "abc123",
	}

	importArg := key.ImportCacheArg()
	assert.Equal(t, "type=registry,ref=localhost:8080/cache/tenant/nodejs/abc123", importArg)

	exportArg := key.ExportCacheArg()
	assert.Equal(t, "type=registry,ref=localhost:8080/cache/tenant/nodejs/abc123,mode=max", exportArg)
}

func TestValidateCacheScope(t *testing.T) {
	tests := []struct {
		scope   string
		wantErr bool
	}{
		{"valid-scope", false},
		{"abc", false},
		{"my-team-123", false},
		{"", true},                       // Empty
		{"ab", true},                     // Too short
		{"a", true},                      // Too short
		{string(make([]byte, 65)), true}, // Too long
	}

	for _, tt := range tests {
		t.Run(tt.scope, func(t *testing.T) {
			err := ValidateCacheScope(tt.scope)
			if tt.wantErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestNormalizeCacheScope(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"simple", "simple"},
		{"with-hyphens", "with-hyphens"},
		{"MixedCase", "mixedcase"},
		{"with spaces", "with-spaces"},
		{"special!@#chars", "special-chars"},
		{"multiple---hyphens", "multiple-hyphens"},
		{"-leading-trailing-", "leading-trailing"},
		{"123numbers", "123numbers"},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			result := normalizeCacheScope(tt.input)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestComputeCombinedHash(t *testing.T) {
	// Same inputs should produce same hash
	hash1 := computeCombinedHash(map[string]string{
		"package-lock.json": "abc123",
		"yarn.lock":         "def456",
	})
	hash2 := computeCombinedHash(map[string]string{
		"yarn.lock":         "def456",
		"package-lock.json": "abc123",
	})
	assert.Equal(t, hash1, hash2, "hash should be deterministic regardless of map order")

	// Different inputs should produce different hashes
	hash3 := computeCombinedHash(map[string]string{
		"package-lock.json": "different",
	})
	assert.NotEqual(t, hash1, hash3)

	// Empty map should return a valid hash (64 hex chars), not a short string
	emptyHash := computeCombinedHash(map[string]string{})
	assert.Len(t, emptyHash, 64, "empty hash should be 64 hex characters (sha256)")

	// Nil map should also return a valid hash
	nilHash := computeCombinedHash(nil)
	assert.Len(t, nilHash, 64, "nil hash should be 64 hex characters (sha256)")
	assert.Equal(t, emptyHash, nilHash, "empty and nil should produce same hash")
}

func TestGetCacheKeyFromConfig(t *testing.T) {
	// With cache scope
	importArg, exportArg, err := GetCacheKeyFromConfig(
		"localhost:8080",
		"my-tenant",
		"nodejs",
		map[string]string{"package-lock.json": "abc"},
	)
	require.NoError(t, err)
	assert.NotEmpty(t, importArg)
	assert.NotEmpty(t, exportArg)
	assert.Contains(t, importArg, "type=registry")
	assert.Contains(t, exportArg, "mode=max")

	// Without cache scope (caching disabled)
	importArg, exportArg, err = GetCacheKeyFromConfig(
		"localhost:8080",
		"", // Empty = no caching
		"nodejs",
		nil,
	)
	require.NoError(t, err)
	assert.Empty(t, importArg)
	assert.Empty(t, exportArg)

	// With cache scope but empty lockfileHashes - should not panic (regression test)
	importArg, exportArg, err = GetCacheKeyFromConfig(
		"localhost:8080",
		"my-tenant",
		"nodejs",
		map[string]string{}, // Empty lockfileHashes
	)
	require.NoError(t, err)
	assert.NotEmpty(t, importArg, "should generate cache args even with empty lockfileHashes")
	assert.NotEmpty(t, exportArg)

	// With cache scope but nil lockfileHashes - should not panic (regression test)
	importArg, exportArg, err = GetCacheKeyFromConfig(
		"localhost:8080",
		"my-tenant",
		"python",
		nil, // nil lockfileHashes
	)
	require.NoError(t, err)
	assert.NotEmpty(t, importArg, "should generate cache args even with nil lockfileHashes")
	assert.NotEmpty(t, exportArg)
}
