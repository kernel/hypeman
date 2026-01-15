package builds

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRegistryTokenGenerator_GeneratePushToken(t *testing.T) {
	generator := NewRegistryTokenGenerator("test-secret-key")

	t.Run("valid token generation", func(t *testing.T) {
		token, err := generator.GeneratePushToken("build-123", []string{"builds/build-123", "cache/tenant-x"}, 30*time.Minute)
		require.NoError(t, err)
		assert.NotEmpty(t, token)

		// Validate the token
		claims, err := generator.ValidateToken(token)
		require.NoError(t, err)
		assert.Equal(t, "build-123", claims.BuildID)
		assert.Equal(t, []string{"builds/build-123", "cache/tenant-x"}, claims.Repositories)
		assert.Equal(t, "push", claims.Scope)
		assert.Equal(t, "builder-build-123", claims.Subject)
		assert.Equal(t, "hypeman", claims.Issuer)
	})

	t.Run("empty build ID", func(t *testing.T) {
		_, err := generator.GeneratePushToken("", []string{"builds/build-123"}, 30*time.Minute)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "build ID is required")
	})

	t.Run("empty repositories", func(t *testing.T) {
		_, err := generator.GeneratePushToken("build-123", []string{}, 30*time.Minute)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "at least one repository is required")
	})
}

func TestRegistryTokenGenerator_ValidateToken(t *testing.T) {
	generator := NewRegistryTokenGenerator("test-secret-key")

	t.Run("valid token", func(t *testing.T) {
		token, err := generator.GeneratePushToken("build-abc", []string{"builds/build-abc"}, time.Hour)
		require.NoError(t, err)

		claims, err := generator.ValidateToken(token)
		require.NoError(t, err)
		assert.Equal(t, "build-abc", claims.BuildID)
	})

	t.Run("expired token", func(t *testing.T) {
		// Generate a token that expires immediately
		token, err := generator.GeneratePushToken("build-expired", []string{"builds/build-expired"}, -time.Hour)
		require.NoError(t, err)

		_, err = generator.ValidateToken(token)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "token is expired")
	})

	t.Run("invalid signature", func(t *testing.T) {
		// Generate with one secret
		gen1 := NewRegistryTokenGenerator("secret-1")
		token, err := gen1.GeneratePushToken("build-123", []string{"builds/build-123"}, time.Hour)
		require.NoError(t, err)

		// Validate with different secret
		gen2 := NewRegistryTokenGenerator("secret-2")
		_, err = gen2.ValidateToken(token)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "signature is invalid")
	})

	t.Run("malformed token", func(t *testing.T) {
		_, err := generator.ValidateToken("not.a.valid.jwt.token")
		require.Error(t, err)
	})
}

func TestRegistryTokenClaims_IsRepositoryAllowed(t *testing.T) {
	claims := &RegistryTokenClaims{
		Repositories: []string{"builds/abc123", "cache/tenant-x"},
	}

	t.Run("allowed repo", func(t *testing.T) {
		assert.True(t, claims.IsRepositoryAllowed("builds/abc123"))
		assert.True(t, claims.IsRepositoryAllowed("cache/tenant-x"))
	})

	t.Run("not allowed repo", func(t *testing.T) {
		assert.False(t, claims.IsRepositoryAllowed("builds/other"))
		assert.False(t, claims.IsRepositoryAllowed("cache/other-tenant"))
	})
}

func TestRegistryTokenClaims_IsPushAllowed(t *testing.T) {
	t.Run("push scope", func(t *testing.T) {
		claims := &RegistryTokenClaims{Scope: "push"}
		assert.True(t, claims.IsPushAllowed())
		assert.True(t, claims.IsPullAllowed()) // push implies pull
	})

	t.Run("pull scope", func(t *testing.T) {
		claims := &RegistryTokenClaims{Scope: "pull"}
		assert.False(t, claims.IsPushAllowed())
		assert.True(t, claims.IsPullAllowed())
	})
}
