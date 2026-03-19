package instances

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBuildEgressProxyInjectRules_AfterSecretUpdate(t *testing.T) {
	t.Parallel()

	egressPolicy := &NetworkEgressPolicy{Enabled: true}
	credentials := map[string]CredentialPolicy{
		"API_KEY": {
			Source: CredentialSource{Env: "API_KEY"},
			Inject: []CredentialInjectRule{
				{
					Hosts: []string{"api.example.com"},
					As: CredentialInjectAs{
						Header: "Authorization",
						Format: "Bearer ${value}",
					},
				},
			},
		},
	}

	// Initial env
	env := map[string]string{"API_KEY": "original-secret"}
	rules := buildEgressProxyInjectRules(egressPolicy, credentials, env)
	require.Len(t, rules, 1)
	assert.Equal(t, "Bearer original-secret", rules[0].HeaderValue)

	// After rotation
	env["API_KEY"] = "rotated-secret"
	rules = buildEgressProxyInjectRules(egressPolicy, credentials, env)
	require.Len(t, rules, 1)
	assert.Equal(t, "Bearer rotated-secret", rules[0].HeaderValue)
}

func TestValidateCredentialEnvBindings_AfterUpdate(t *testing.T) {
	t.Parallel()

	credentials := map[string]CredentialPolicy{
		"API_KEY": {
			Source: CredentialSource{Env: "API_KEY"},
			Inject: []CredentialInjectRule{
				{As: CredentialInjectAs{Header: "X-Key", Format: "${value}"}},
			},
		},
	}

	// Valid update
	env := map[string]string{"API_KEY": "new-key"}
	assert.NoError(t, validateCredentialEnvBindings(credentials, env))

	// Empty value
	env["API_KEY"] = "  "
	assert.Error(t, validateCredentialEnvBindings(credentials, env))

	// Missing key
	delete(env, "API_KEY")
	assert.Error(t, validateCredentialEnvBindings(credentials, env))
}
