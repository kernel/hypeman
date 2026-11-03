package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoadJWTVerificationSecrets(t *testing.T) {
	t.Setenv("JWT_SECRET", "")
	path := filepath.Join(t.TempDir(), "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte("jwt_secret: primary\njwt_verification_secrets: [old, older]\n"), 0600))
	for _, tc := range []struct {
		name string
		env  string
		want []string
	}{
		{"yaml", "", []string{"old", "older"}},
		{"environment overrides yaml", `["next","another"]`, []string{"next", "another"}},
		{"environment clears yaml", `[]`, []string{}},
		{"commas remain part of a key", `["key,with,commas"]`, []string{"key,with,commas"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("JWT_VERIFICATION_SECRETS", tc.env)
			cfg, err := Load(path)
			require.NoError(t, err)
			require.NoError(t, cfg.Validate())
			assert.Equal(t, "primary", cfg.JwtSecret)
			assert.Equal(t, tc.want, cfg.JwtVerificationSecrets)
		})
	}
}

func TestLoadJWTVerificationSecretsRejectsInvalidEnv(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte("{}\n"), 0600))
	for _, value := range []string{`old,new`, `null`, `"secret"`, `[123]`, `["secret"`, `[""]`, `[null]`} {
		t.Run(value, func(t *testing.T) {
			t.Setenv("JWT_VERIFICATION_SECRETS", value)
			cfg, err := Load(path)
			if err == nil {
				err = cfg.Validate()
			}
			require.Error(t, err)
			assert.NotContains(t, err.Error(), "\"secret\"")
		})
	}
}

func TestValidateJWTVerificationSecretsRejectsEmptyEntry(t *testing.T) {
	cfg := defaultConfig()
	cfg.JwtVerificationSecrets = []string{"old", ""}
	require.ErrorContains(t, cfg.Validate(), "entry 1 must not be empty")
}
