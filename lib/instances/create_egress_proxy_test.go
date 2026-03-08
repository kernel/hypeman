package instances

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestValidateCreateRequest_EgressProxyRequiresNetwork(t *testing.T) {
	t.Parallel()

	req := CreateInstanceRequest{
		Name:           "test-egress",
		Image:          "docker.io/library/alpine:latest",
		NetworkEnabled: false,
		Env: map[string]string{
			"OUTBOUND_OPENAI_KEY": "real-key",
		},
		EgressProxy: &EgressProxyConfig{
			Enabled:     true,
			MockEnvVars: []string{"OUTBOUND_OPENAI_KEY"},
		},
	}

	err := validateCreateRequest(req)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrInvalidRequest)
	assert.Contains(t, err.Error(), "network_enabled=true")
}

func TestValidateCreateRequest_EgressProxyMissingEnvVar(t *testing.T) {
	t.Parallel()

	req := CreateInstanceRequest{
		Name:           "test-egress",
		Image:          "docker.io/library/alpine:latest",
		NetworkEnabled: true,
		Env:            map[string]string{},
		EgressProxy: &EgressProxyConfig{
			Enabled:     true,
			MockEnvVars: []string{"OUTBOUND_OPENAI_KEY"},
		},
	}

	err := validateCreateRequest(req)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrInvalidRequest)
	assert.Contains(t, err.Error(), "must be present in env")
}

func TestValidateCreateRequest_EgressProxyRejectsEmptyEnvValue(t *testing.T) {
	t.Parallel()

	req := CreateInstanceRequest{
		Name:           "test-egress",
		Image:          "docker.io/library/alpine:latest",
		NetworkEnabled: true,
		Env: map[string]string{
			"OUTBOUND_OPENAI_KEY": "  ",
		},
		EgressProxy: &EgressProxyConfig{
			Enabled:     true,
			MockEnvVars: []string{"OUTBOUND_OPENAI_KEY"},
		},
	}

	err := validateCreateRequest(req)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrInvalidRequest)
	assert.Contains(t, err.Error(), "must be non-empty")
}

func TestValidateCreateRequest_EgressProxyRejectsEmptyMockEnvVarEntry(t *testing.T) {
	t.Parallel()

	req := CreateInstanceRequest{
		Name:           "test-egress",
		Image:          "docker.io/library/alpine:latest",
		NetworkEnabled: true,
		Env: map[string]string{
			"OUTBOUND_OPENAI_KEY": "real-key",
		},
		EgressProxy: &EgressProxyConfig{
			Enabled:     true,
			MockEnvVars: []string{"   "},
		},
	}

	err := validateCreateRequest(req)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrInvalidRequest)
	assert.Contains(t, err.Error(), "must be non-empty")
}

func TestValidateCreateRequest_EgressProxyDedupesMockEnvVars(t *testing.T) {
	t.Parallel()

	cfg := &EgressProxyConfig{
		Enabled: true,
		MockEnvVars: []string{
			"OUTBOUND_OPENAI_KEY",
			" OUTBOUND_OPENAI_KEY ",
			"GITHUB_TOKEN",
		},
	}
	req := CreateInstanceRequest{
		Name:           "test-egress",
		Image:          "docker.io/library/alpine:latest",
		NetworkEnabled: true,
		Env: map[string]string{
			"OUTBOUND_OPENAI_KEY": "real-key",
			"GITHUB_TOKEN":        "real-gh-token",
		},
		EgressProxy: cfg,
	}

	err := validateCreateRequest(req)
	require.NoError(t, err)
	assert.Equal(t, []string{"OUTBOUND_OPENAI_KEY", "GITHUB_TOKEN"}, cfg.MockEnvVars)
}
