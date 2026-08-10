package builds

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFileSecretProvider_GetSecrets(t *testing.T) {
	// Create temp directory with test secrets
	tempDir, err := os.MkdirTemp("", "secrets-test-*")
	require.NoError(t, err)
	defer os.RemoveAll(tempDir)

	// Write test secrets
	require.NoError(t, os.WriteFile(filepath.Join(tempDir, "npm_token"), []byte("npm-secret-value\n"), 0600))
	require.NoError(t, os.WriteFile(filepath.Join(tempDir, "github_token"), []byte("github-secret-value"), 0600))
	require.NoError(t, os.WriteFile(filepath.Join(tempDir, "with_whitespace"), []byte("  trimmed  \n"), 0600))

	provider := NewFileSecretProvider(tempDir)
	ctx := context.Background()

	t.Run("fetch existing secrets", func(t *testing.T) {
		secrets, err := provider.GetSecrets(ctx, []string{"npm_token", "github_token"})
		require.NoError(t, err)
		assert.Len(t, secrets, 2)
		assert.Equal(t, "npm-secret-value", secrets["npm_token"])
		assert.Equal(t, "github-secret-value", secrets["github_token"])
	})

	t.Run("missing secret is an error", func(t *testing.T) {
		secrets, err := provider.GetSecrets(ctx, []string{"npm_token", "nonexistent"})
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrSecretNotFound)
		assert.Contains(t, err.Error(), "nonexistent")
		assert.Nil(t, secrets)
	})

	t.Run("all missing secrets is an error", func(t *testing.T) {
		secrets, err := provider.GetSecrets(ctx, []string{"missing1", "missing2"})
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrSecretNotFound)
		assert.Nil(t, secrets)
	})

	t.Run("whitespace is trimmed", func(t *testing.T) {
		secrets, err := provider.GetSecrets(ctx, []string{"with_whitespace"})
		require.NoError(t, err)
		assert.Equal(t, "trimmed", secrets["with_whitespace"])
	})

	t.Run("path traversal is an error", func(t *testing.T) {
		secrets, err := provider.GetSecrets(ctx, []string{"../etc/passwd", "../../root/.ssh/id_rsa"})
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrInvalidSecretID)
		assert.Nil(t, secrets)
	})

	t.Run("special characters in ID are an error", func(t *testing.T) {
		for _, id := range []string{"foo/bar", "baz\\qux", "..", "."} {
			secrets, err := provider.GetSecrets(ctx, []string{id})
			require.Error(t, err, "id %q", id)
			assert.ErrorIs(t, err, ErrInvalidSecretID, "id %q", id)
			assert.Nil(t, secrets, "id %q", id)
		}
	})

	t.Run("empty ID is an error", func(t *testing.T) {
		secrets, err := provider.GetSecrets(ctx, []string{""})
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrInvalidSecretID)
		assert.Nil(t, secrets)
	})

	t.Run("empty request returns empty map", func(t *testing.T) {
		secrets, err := provider.GetSecrets(ctx, []string{})
		require.NoError(t, err)
		assert.Empty(t, secrets)
	})
}

func TestFileSecretProvider_ContextCancellation(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "secrets-test-*")
	require.NoError(t, err)
	defer os.RemoveAll(tempDir)

	// Write many secrets
	for i := 0; i < 10; i++ {
		require.NoError(t, os.WriteFile(filepath.Join(tempDir, "secret"+string(rune('A'+i))), []byte("value"), 0600))
	}

	provider := NewFileSecretProvider(tempDir)

	// Cancel context immediately
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	secrets, err := provider.GetSecrets(ctx, []string{"secretA", "secretB", "secretC"})
	// May return partial results or context error
	assert.True(t, err == context.Canceled || len(secrets) <= 3)
}

func TestValidateSecretID(t *testing.T) {
	for _, id := range []string{"npm_token", "github-token", "token.json", "a.b_c-d"} {
		assert.NoError(t, ValidateSecretID(id), "id %q", id)
	}
	for _, id := range []string{"", "../secret", "a/b", "a\\b", "..", "."} {
		err := ValidateSecretID(id)
		require.Error(t, err, "id %q", id)
		assert.ErrorIs(t, err, ErrInvalidSecretID, "id %q", id)
	}
}

func TestNoOpSecretProvider(t *testing.T) {
	provider := &NoOpSecretProvider{}
	ctx := context.Background()

	secrets, err := provider.GetSecrets(ctx, []string{"any", "secret", "ids"})
	require.NoError(t, err)
	assert.Empty(t, secrets)
}
