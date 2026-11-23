package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/kernel/hypeman/lib/oidcfederation"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const oidcYAML = `
oidc:
  issuers:
    - name: github-actions
      preset_type: github_actions
    - name: cluster
      preset_type: kubernetes
      issuer_url: https://cluster.example.test
      token_validation:
        max_lifetime_seconds: 600
  federation_rules:
    - name: ci
      issuer: github-actions
      expected_audience: https://hypeman.example.test
      match:
        type: github_actions
        repository_id: 12
        repository_owner_id: "34"
        workflow: .github/workflows/test.yml
        ref: refs/heads/main
        event_names: [schedule]
        claims:
          /runner_environment: github-hosted
      authorization:
        permissions: [instance:read, instance:write]
    - name: pods
      issuer: cluster
      expected_audience: https://hypeman.example.test
      match:
        type: kubernetes
        namespace: ci
        service_account: runner
        claims:
          /kubernetes.io/namespace: ci
      authorization:
        permissions: [image:read]
        max_token_ttl_seconds: 120
`

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte(body), 0600))
	return path
}

func TestLoadOIDC(t *testing.T) {
	t.Setenv("OIDC", "")
	cfg, err := Load(writeConfig(t, oidcYAML))
	require.NoError(t, err)
	require.NoError(t, cfg.Validate())
	registry, err := oidcfederation.Compile(cfg.OIDC)
	require.NoError(t, err)

	ci, ok := registry.Rule("ci")
	require.True(t, ok)
	assert.Equal(t, oidcfederation.GitHubIssuer, ci.IssuerSpec.IssuerURL)
	assert.Equal(t, "12", ci.Match.RepositoryID, "an unquoted numeric ID is read as text")
	assert.Equal(t, []string{"schedule"}, ci.Match.EventNames)
	assert.Equal(t, "github-hosted", ci.Match.Claims["/runner_environment"])
	assert.Equal(t, []string{"instance:read", "instance:write"}, ci.Authorization.Permissions)

	pods, ok := registry.Rule("pods")
	require.True(t, ok)
	assert.Equal(t, "ci", pods.Match.Claims["/kubernetes.io/namespace"], "a dotted claim pointer is one key")
	assert.Equal(t, 600, *pods.IssuerSpec.TokenValidation.MaxLifetimeSeconds)
	assert.Equal(t, 120, *pods.Authorization.MaxTokenTTLSeconds)
}

func TestLoadWithoutOIDC(t *testing.T) {
	t.Setenv("OIDC", "")
	cfg, err := Load(writeConfig(t, "{}\n"))
	require.NoError(t, err)
	require.NoError(t, cfg.Validate())
	registry, err := oidcfederation.Compile(cfg.OIDC)
	require.NoError(t, err)
	assert.False(t, registry.Enabled())
}

// The environment replaces the file's block; it is not merged into it.
func TestLoadOIDCFromEnvironment(t *testing.T) {
	t.Setenv("OIDC", `{"issuers":[{"name":"idp","issuer_url":"https://idp.example.test"}],"federation_rules":[{"name":"deploy","issuer":"idp","expected_audience":"https://hypeman.example.test","match":{"type":"custom_oidc","subject":"deployer"},"authorization":{"permissions":["image:write"],"max_token_ttl_seconds":60}}]}`)
	cfg, err := Load(writeConfig(t, oidcYAML))
	require.NoError(t, err)
	require.NoError(t, cfg.Validate())
	registry, err := oidcfederation.Compile(cfg.OIDC)
	require.NoError(t, err)
	_, ok := registry.Rule("ci")
	assert.False(t, ok, "rules from the file do not survive the override")
	deploy, ok := registry.Rule("deploy")
	require.True(t, ok)
	assert.Equal(t, 60, *deploy.Authorization.MaxTokenTTLSeconds)

	t.Setenv("OIDC", `{}`)
	cfg, err = Load(writeConfig(t, oidcYAML))
	require.NoError(t, err)
	assert.Empty(t, cfg.OIDC.FederationRules, "an empty object turns federation off")
}

func TestLoadOIDCRejectsInvalidConfig(t *testing.T) {
	for name, body := range map[string]string{
		// A misspelled restriction must not be dropped silently.
		"unknown rule key":  "oidc:\n  issuers: [{name: idp, issuer_url: 'https://idp.example.test'}]\n  federation_rules:\n    - name: r\n      issuer: idp\n      expected_audience: a\n      match: {type: custom_oidc, subject: s, subject_prefx: t}\n      authorization: {permissions: [image:read]}\n",
		"unknown block key": "oidc:\n  issuer: []\n",
		// A repository name is pinned through claims; a field the matcher ignored would look enforced.
		"repository name key": "oidc:\n  issuers: [{name: gh, preset_type: github_actions}]\n  federation_rules:\n    - name: r\n      issuer: gh\n      expected_audience: a\n      match: {type: github_actions, repository: owner/name, repository_id: 1, repository_owner_id: 2, workflow: .github/workflows/ci.yml, ref: refs/heads/main}\n      authorization: {permissions: [image:read]}\n",
		"unknown issuer":      "oidc:\n  federation_rules:\n    - name: r\n      issuer: missing\n      expected_audience: a\n      match: {type: custom_oidc, subject: s}\n      authorization: {permissions: [image:read]}\n",
		"no permissions":      "oidc:\n  issuers: [{name: idp, issuer_url: 'https://idp.example.test'}]\n  federation_rules:\n    - name: r\n      issuer: idp\n      expected_audience: a\n      match: {type: custom_oidc, subject: s}\n",
		"wildcard permission": "oidc:\n  issuers: [{name: idp, issuer_url: 'https://idp.example.test'}]\n  federation_rules:\n    - name: r\n      issuer: idp\n      expected_audience: a\n      match: {type: custom_oidc, subject: s}\n      authorization: {permissions: ['*']}\n",
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("OIDC", "")
			cfg, err := Load(writeConfig(t, body))
			if err == nil {
				err = cfg.Validate()
			}
			require.Error(t, err)
		})
	}
	for _, value := range []string{`[]`, `null`, `"oidc"`, `{"issuers":`, `{"unknown":true}`} {
		t.Run(value, func(t *testing.T) {
			t.Setenv("OIDC", value)
			_, err := Load(writeConfig(t, "{}\n"))
			require.Error(t, err)
		})
	}
}
