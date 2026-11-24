package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/kernel/hypeman/lib/accesstoken"
	"github.com/stretchr/testify/require"
)

func loadYAML(t *testing.T, body string) (*Config, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
	return Load(path)
}

func TestLoadOAuth(t *testing.T) {
	cfg, err := loadYAML(t, `
oauth:
  audiences: [prod, https://host-1.example.test]
  authorization_servers:
    - issuer: https://auth.example.test
    - issuer: https://other.example.test
      jwks:
        type: explicit_url
        url: https://other.example.test/keys.json
`)
	require.NoError(t, err)
	require.NoError(t, cfg.Validate())
	require.Equal(t, []string{"prod", "https://host-1.example.test"}, cfg.OAuth.Audiences)
	require.Len(t, cfg.OAuth.AuthorizationServers, 2)
	require.Equal(t, "https://auth.example.test", cfg.OAuth.AuthorizationServers[0].Issuer)
	require.Equal(t, "https://other.example.test/keys.json", cfg.OAuth.AuthorizationServers[1].JWKS.URL)

	// Without the block nothing is configured.
	cfg, err = loadYAML(t, "port: 4973\n")
	require.NoError(t, err)
	require.NoError(t, cfg.Validate())
	require.Empty(t, cfg.OAuth.AuthorizationServers)
}

func TestOAuthRejectsUnknownKeysAndInvalidSettings(t *testing.T) {
	for _, body := range []string{
		"oauth:\n  audience: [prod]\n",
		"oauth:\n  authorization_servers:\n    - issuer: https://auth.example.test\n      jwks_url: https://auth.example.test/keys\n",
		"oauth:\n  authorization_servers:\n    - issuer: https://auth.example.test\n      jwks:\n        kind: discovery\n",
	} {
		_, err := loadYAML(t, body)
		require.ErrorContains(t, err, "invalid oauth config", body)
	}
	for _, body := range []string{
		"oauth:\n  authorization_servers:\n    - issuer: https://auth.example.test\n",
		"oauth:\n  audiences: [prod]\n",
		"oauth:\n  audiences: [prod]\n  authorization_servers:\n    - issuer: http://auth.example.test\n",
	} {
		cfg, err := loadYAML(t, body)
		require.NoError(t, err, body)
		require.ErrorIs(t, cfg.Validate(), accesstoken.ErrInvalidConfig, body)
	}
}
