package instances

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestValidateCreateRequest_NetworkEgressRequiresNetwork(t *testing.T) {
	t.Parallel()

	req := CreateInstanceRequest{
		Name:           "test-egress",
		Image:          "docker.io/library/alpine:latest",
		NetworkEnabled: false,
		NetworkEgress:  &NetworkEgressPolicy{Enabled: true},
	}

	err := validateCreateRequest(req)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrInvalidRequest)
	assert.Contains(t, err.Error(), "network.egress requires network.enabled=true")
}

func TestValidateCreateRequest_CredentialsRequireNetworkEgress(t *testing.T) {
	t.Parallel()

	req := CreateInstanceRequest{
		Name:           "test-credentials",
		Image:          "docker.io/library/alpine:latest",
		NetworkEnabled: true,
		Env: map[string]string{
			"OUTBOUND_OPENAI_KEY": "real-key",
		},
		Credentials: map[string]CredentialPolicy{
			"OUTBOUND_OPENAI_KEY": {
				Source: CredentialSource{Env: "OUTBOUND_OPENAI_KEY"},
				Inject: []CredentialInjectRule{
					{As: CredentialInjectAs{Header: "Authorization", Format: "Bearer ${value}"}},
				},
			},
		},
	}

	err := validateCreateRequest(req)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrInvalidRequest)
	assert.Contains(t, err.Error(), "credentials require network.egress.enabled=true")
}

func TestValidateCreateRequest_CredentialSourceEnvMustExist(t *testing.T) {
	t.Parallel()

	req := CreateInstanceRequest{
		Name:           "test-credentials",
		Image:          "docker.io/library/alpine:latest",
		NetworkEnabled: true,
		NetworkEgress:  &NetworkEgressPolicy{Enabled: true},
		Env:            map[string]string{},
		Credentials: map[string]CredentialPolicy{
			"OUTBOUND_OPENAI_KEY": {
				Source: CredentialSource{Env: "OUTBOUND_OPENAI_KEY"},
				Inject: []CredentialInjectRule{
					{As: CredentialInjectAs{Header: "Authorization", Format: "Bearer ${value}"}},
				},
			},
		},
	}

	err := validateCreateRequest(req)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrInvalidRequest)
	assert.Contains(t, err.Error(), "must be present in env")
}

func TestValidateCreateRequest_CredentialSourceEnvMustBeNonEmpty(t *testing.T) {
	t.Parallel()

	req := CreateInstanceRequest{
		Name:           "test-credentials",
		Image:          "docker.io/library/alpine:latest",
		NetworkEnabled: true,
		NetworkEgress:  &NetworkEgressPolicy{Enabled: true},
		Env: map[string]string{
			"OUTBOUND_OPENAI_KEY": "   ",
		},
		Credentials: map[string]CredentialPolicy{
			"OUTBOUND_OPENAI_KEY": {
				Source: CredentialSource{Env: "OUTBOUND_OPENAI_KEY"},
				Inject: []CredentialInjectRule{
					{As: CredentialInjectAs{Header: "Authorization", Format: "Bearer ${value}"}},
				},
			},
		},
	}

	err := validateCreateRequest(req)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrInvalidRequest)
	assert.Contains(t, err.Error(), "must be non-empty")
}

func TestValidateCreateRequest_RejectsInvalidEgressEnforcementMode(t *testing.T) {
	t.Parallel()

	req := CreateInstanceRequest{
		Name:           "test-egress",
		Image:          "docker.io/library/alpine:latest",
		NetworkEnabled: true,
		NetworkEgress: &NetworkEgressPolicy{
			Enabled:         true,
			EnforcementMode: EgressEnforcementMode("bogus"),
		},
	}

	err := validateCreateRequest(req)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrInvalidRequest)
	assert.Contains(t, err.Error(), "invalid network.egress.enforcement.mode")
}

func TestValidateCreateRequest_AllowsHTTPHTTPSOnlyEgressMode(t *testing.T) {
	t.Parallel()

	cfg := &NetworkEgressPolicy{
		Enabled:         true,
		EnforcementMode: EgressEnforcementModeHTTPHTTPSOnly,
	}
	req := CreateInstanceRequest{
		Name:           "test-egress",
		Image:          "docker.io/library/alpine:latest",
		NetworkEnabled: true,
		NetworkEgress:  cfg,
	}

	err := validateCreateRequest(req)
	require.NoError(t, err)
	assert.Equal(t, EgressEnforcementModeHTTPHTTPSOnly, cfg.EnforcementMode)
}

func TestNormalizeCredentialPolicies_RejectsInvalidHostPattern(t *testing.T) {
	t.Parallel()

	_, err := normalizeCredentialPolicies(map[string]CredentialPolicy{
		"OUTBOUND_OPENAI_KEY": {
			Source: CredentialSource{Env: "OUTBOUND_OPENAI_KEY"},
			Inject: []CredentialInjectRule{
				{
					Hosts: []string{"https://api.openai.com"},
					As:    CredentialInjectAs{Header: "Authorization", Format: "Bearer ${value}"},
				},
			},
		},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid host pattern")
}

func TestNormalizeCredentialPolicies_DedupesAndNormalizesHosts(t *testing.T) {
	t.Parallel()

	normalized, err := normalizeCredentialPolicies(map[string]CredentialPolicy{
		"OUTBOUND_OPENAI_KEY": {
			Source: CredentialSource{Env: "OUTBOUND_OPENAI_KEY"},
			Inject: []CredentialInjectRule{
				{
					Hosts: []string{
						" API.OpenAI.com ",
						"*.OpenAI.com",
						"api.openai.com",
					},
					As: CredentialInjectAs{Header: "Authorization", Format: "Bearer ${value}"},
				},
			},
		},
	})
	require.NoError(t, err)
	policy := normalized["OUTBOUND_OPENAI_KEY"]
	require.Len(t, policy.Inject, 1)
	assert.Equal(t, []string{"api.openai.com", "*.openai.com"}, policy.Inject[0].Hosts)
}

func TestNormalizeCredentialPolicies_RejectsMissingValueTemplate(t *testing.T) {
	t.Parallel()

	_, err := normalizeCredentialPolicies(map[string]CredentialPolicy{
		"OUTBOUND_OPENAI_KEY": {
			Source: CredentialSource{Env: "OUTBOUND_OPENAI_KEY"},
			Inject: []CredentialInjectRule{
				{
					As: CredentialInjectAs{Header: "Authorization", Format: "Bearer token"},
				},
			},
		},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "must include ${value}")
}
