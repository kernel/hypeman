package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

const fileConfig = `
issuer: https://auth.example.test
otel_endpoint: http://collector.example.test:4317
signing_key:
  file: /etc/hypeman-auth/signing-key.pem
published_keys:
  - pem: previous
replay_store:
  postgres_url: postgres://file
oidc:
  issuers:
    - name: github-actions
      preset_type: github_actions
  federation_rules:
    - name: ci
      issuer: github-actions
      expected_audience: https://auth.example.test
      match:
        type: github_actions
        repository_id: "12"
        repository_owner_id: "34"
        workflow: .github/workflows/test.yml
        ref: refs/heads/main
      authorization:
        audiences: [https://hypeman.example.test]
        permissions: [instance:read]
        subject_claim: /actor
`

func load(t *testing.T, body string) (*Config, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
	return Load(path)
}

func TestLoadFile(t *testing.T) {
	cfg, err := load(t, fileConfig)
	require.NoError(t, err)
	require.Equal(t, "https://auth.example.test", cfg.Issuer)
	require.Equal(t, ":8080", cfg.ListenAddress)
	require.Empty(t, cfg.MetricsListenAddress)
	require.Equal(t, Key{File: "/etc/hypeman-auth/signing-key.pem"}, cfg.SigningKey)
	require.Equal(t, []Key{{PEM: "previous"}}, cfg.PublishedKeys)
	require.Equal(t, "postgres://file", cfg.ReplayStore.PostgresURL)
	require.Equal(t, "http://collector.example.test:4317", cfg.OtelEndpoint)
	require.Len(t, cfg.OIDC.FederationRules, 1)
	rule := cfg.OIDC.FederationRules[0]
	require.Equal(t, "12", rule.Match.RepositoryID)
	require.Equal(t, []string{"https://hypeman.example.test"}, rule.Authorization.Audiences)
	require.Equal(t, "/actor", rule.Authorization.SubjectClaim)
}

// A deployment without a file is configured by the environment alone, and
// the environment wins over a file.
func TestEnvironmentOverridesTheFile(t *testing.T) {
	t.Setenv("ISSUER", "https://env.example.test")
	t.Setenv("LISTEN_ADDRESS", ":9000")
	t.Setenv("SIGNING_KEY__PEM", "from the environment")
	t.Setenv("REPLAY_STORE__POSTGRES_URL", "postgres://env")
	t.Setenv("OTEL_ENDPOINT", "https://env.example.test:4317")
	t.Setenv("PUBLISHED_KEYS", `[{"pem":"one"},{"file":"/keys/two.pem"}]`)
	t.Setenv("OIDC", `{"issuers":[{"name":"cluster","preset_type":"kubernetes","issuer_url":"https://cluster.example.test"}]}`)
	// The rest of the environment is not configuration.
	t.Setenv("HOME_DIRECTORY_OF_SOMETHING", "/tmp")

	for _, withFile := range []bool{false, true} {
		cfg, err := Load("")
		if withFile {
			cfg, err = load(t, fileConfig)
		}
		require.NoError(t, err)
		require.Equal(t, "https://env.example.test", cfg.Issuer)
		require.Equal(t, ":9000", cfg.ListenAddress)
		require.Equal(t, "from the environment", cfg.SigningKey.PEM)
		require.Equal(t, "postgres://env", cfg.ReplayStore.PostgresURL)
		require.Equal(t, "https://env.example.test:4317", cfg.OtelEndpoint)
		require.Equal(t, []Key{{PEM: "one"}, {File: "/keys/two.pem"}}, cfg.PublishedKeys)
		// The block from the environment replaces the file's as a whole.
		require.Len(t, cfg.OIDC.Issuers, 1)
		require.Equal(t, "cluster", cfg.OIDC.Issuers[0].Name)
		require.Empty(t, cfg.OIDC.FederationRules)
	}
}

func TestUnknownKeysAreErrors(t *testing.T) {
	for _, body := range []string{
		"isuer: https://auth.example.test\n",
		"replay_store:\n  postgres: postgres://x\n",
		"signing_key:\n  path: /key.pem\n",
		"oidc:\n  federation_rules:\n    - name: ci\n      match:\n        repository: kernel/hypeman\n",
		"oidc:\n  federation_rules:\n    - name: ci\n      authorization:\n        audience: [prod]\n",
	} {
		_, err := load(t, body)
		require.ErrorContains(t, err, "invalid config", body)
	}
	t.Setenv("OIDC", `{"rules":[]}`)
	_, err := Load("")
	require.ErrorContains(t, err, "invalid config")
	t.Setenv("OIDC", `not json`)
	_, err = Load("")
	require.ErrorContains(t, err, "OIDC must be a JSON document")

	_, err = Load(filepath.Join(t.TempDir(), "missing.yaml"))
	require.Error(t, err)

	// A mistake in the file shows even in a block the environment replaces.
	t.Setenv("OIDC", `{"issuers":[]}`)
	_, err = load(t, "oidc:\n  isuers: []\n")
	require.ErrorContains(t, err, "invalid config")
}

// A value must have the type its key takes. A number written for a text
// value is not converted: 9007199254740993 would arrive as 9007199254740992
// and the rule would name another account.
func TestValuesAreNotConvertedBetweenTypes(t *testing.T) {
	rule := func(match, authorization string) string {
		return `{"issuers":[{"name":"idp","issuer_url":"https://idp.example.test"}],"federation_rules":[{"name":"ci","issuer":"idp","expected_audience":"https://auth.example.test","match":{"type":"custom_oidc",` + match + `},"authorization":{"audiences":["prod"],"permissions":["instance:read"]` + authorization + `}}]}`
	}
	t.Setenv("OIDC", rule(`"subject":"9007199254740993"`, `,"max_token_ttl_seconds":120`))
	cfg, err := Load("")
	require.NoError(t, err)
	require.Equal(t, "9007199254740993", cfg.OIDC.FederationRules[0].Match.Subject)
	require.Equal(t, 120, *cfg.OIDC.FederationRules[0].Authorization.MaxTokenTTLSeconds)

	for _, document := range []string{
		rule(`"subject":9007199254740993`, ""),
		rule(`"subject":"build","claims":{"/account":9007199254740993}`, ""),
		rule(`"subject":"build","claims":{"/verified":true}`, ""),
		rule(`"subject":"build"`, `,"max_token_ttl_seconds":"120"`),
	} {
		t.Setenv("OIDC", document)
		_, err := Load("")
		require.ErrorContains(t, err, "invalid config", document)
	}
	t.Setenv("OIDC", "")
	for _, body := range []string{
		"oidc:\n  federation_rules:\n    - name: ci\n      match:\n        repository_id: 12\n",
		"oidc:\n  federation_rules:\n    - name: ci\n      match:\n        subject: 112345678901234567890\n",
		"listen_address: 8080\n",
	} {
		_, err := load(t, body)
		require.ErrorContains(t, err, "invalid config", body)
	}
	cfg, err = load(t, "oidc:\n  federation_rules:\n    - name: ci\n      authorization:\n        max_token_ttl_seconds: 120\n")
	require.NoError(t, err)
	require.Equal(t, 120, *cfg.OIDC.FederationRules[0].Authorization.MaxTokenTTLSeconds)
}

func TestKeyRead(t *testing.T) {
	path := filepath.Join(t.TempDir(), "key.pem")
	require.NoError(t, os.WriteFile(path, []byte("from a file"), 0o600))
	encoded, err := Key{File: path}.Read()
	require.NoError(t, err)
	require.Equal(t, "from a file", string(encoded))
	encoded, err = Key{PEM: "inline"}.Read()
	require.NoError(t, err)
	require.Equal(t, "inline", string(encoded))
	for _, key := range []Key{{}, {PEM: "inline", File: path}, {File: filepath.Join(t.TempDir(), "missing.pem")}} {
		_, err := key.Read()
		require.Error(t, err)
	}
}
