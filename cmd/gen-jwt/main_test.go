package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGetJWTSecretUsesOnlySigningKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte("jwt_secret: new\njwt_verification_secrets: [old]\n"), 0600))
	t.Setenv("CONFIG_PATH", path)
	t.Setenv("JWT_VERIFICATION_SECRETS", `["another"]`)

	t.Setenv("JWT_SECRET", "")
	assert.Equal(t, "new", getJWTSecret())

	t.Setenv("JWT_SECRET", "env-primary")
	assert.Equal(t, "env-primary", getJWTSecret())
}
