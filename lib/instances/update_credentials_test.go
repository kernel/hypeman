package instances

import (
	"os"
	"testing"

	"github.com/kernel/hypeman/lib/paths"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// setupCredentialsTestManager creates a minimal manager with temp paths and prepopulated metadata.
func setupCredentialsTestManager(t *testing.T, meta *metadata) *manager {
	t.Helper()
	tmpDir := t.TempDir()
	p := paths.New(tmpDir)

	m := &manager{paths: p}

	// Create instance directory so saveMetadata can write
	require.NoError(t, os.MkdirAll(p.InstanceDir(meta.Id), 0755))
	require.NoError(t, m.saveMetadata(meta))

	return m
}

func TestUpdateCredentials_ValidatesEgressRequired(t *testing.T) {
	t.Parallel()

	m := setupCredentialsTestManager(t, &metadata{
		StoredMetadata: StoredMetadata{
			Id:             "test-inst",
			Name:           "test",
			NetworkEnabled: true,
			NetworkEgress:  nil,
			Env:            map[string]string{},
		},
	})

	_, err := m.updateCredentials(t.Context(), "test-inst", UpdateCredentialsRequest{
		Credentials: map[string]CredentialPolicy{
			"MY_KEY": {
				Source: CredentialSource{Env: "MY_KEY"},
				Inject: []CredentialInjectRule{
					{As: CredentialInjectAs{Header: "Authorization", Format: "Bearer ${value}"}},
				},
			},
		},
		Env: map[string]string{"MY_KEY": "real-secret"},
	})
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrInvalidRequest)
	assert.Contains(t, err.Error(), "credentials require network.egress.enabled=true")
}

func TestUpdateCredentials_ValidatesEnvBinding(t *testing.T) {
	t.Parallel()

	m := setupCredentialsTestManager(t, &metadata{
		StoredMetadata: StoredMetadata{
			Id:             "test-inst",
			Name:           "test",
			NetworkEnabled: true,
			NetworkEgress:  &NetworkEgressPolicy{Enabled: true},
			Env:            map[string]string{},
		},
	})

	_, err := m.updateCredentials(t.Context(), "test-inst", UpdateCredentialsRequest{
		Credentials: map[string]CredentialPolicy{
			"MY_KEY": {
				Source: CredentialSource{Env: "MY_KEY"},
				Inject: []CredentialInjectRule{
					{As: CredentialInjectAs{Header: "Authorization", Format: "Bearer ${value}"}},
				},
			},
		},
		Env: map[string]string{},
	})
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrInvalidRequest)
	assert.Contains(t, err.Error(), "must be present in env")
}

func TestUpdateCredentials_PreservesExistingWhenNilRequest(t *testing.T) {
	t.Parallel()

	m := setupCredentialsTestManager(t, &metadata{
		StoredMetadata: StoredMetadata{
			Id:             "test-inst",
			Name:           "test",
			NetworkEnabled: true,
			NetworkEgress:  &NetworkEgressPolicy{Enabled: true},
			Env:            map[string]string{"MY_KEY": "old-secret"},
			Credentials: map[string]CredentialPolicy{
				"MY_KEY": {
					Source: CredentialSource{Env: "MY_KEY"},
					Inject: []CredentialInjectRule{
						{As: CredentialInjectAs{Header: "Authorization", Format: "Bearer ${value}"}},
					},
				},
			},
		},
	})

	// PATCH with nil credentials should preserve existing credentials
	result, err := m.updateCredentials(t.Context(), "test-inst", UpdateCredentialsRequest{
		Credentials: nil,
		Env:         map[string]string{},
	})
	require.NoError(t, err)
	require.NotNil(t, result)

	reloaded, err := m.loadMetadata("test-inst")
	require.NoError(t, err)
	require.Len(t, reloaded.StoredMetadata.Credentials, 1)
	_, ok := reloaded.StoredMetadata.Credentials["MY_KEY"]
	assert.True(t, ok, "existing credential should be preserved")
}

func TestUpdateCredentials_MergesNewCredentialWithExisting(t *testing.T) {
	t.Parallel()

	m := setupCredentialsTestManager(t, &metadata{
		StoredMetadata: StoredMetadata{
			Id:             "test-inst",
			Name:           "test",
			NetworkEnabled: true,
			NetworkEgress:  &NetworkEgressPolicy{Enabled: true},
			Env: map[string]string{
				"OPENAI_KEY":  "openai-secret",
				"ANTHROPIC_KEY": "anthropic-secret",
			},
			Credentials: map[string]CredentialPolicy{
				"OPENAI_KEY": {
					Source: CredentialSource{Env: "OPENAI_KEY"},
					Inject: []CredentialInjectRule{
						{
							Hosts: []string{"api.openai.com"},
							As:    CredentialInjectAs{Header: "Authorization", Format: "Bearer ${value}"},
						},
					},
				},
			},
		},
	})

	// PATCH: add a new credential without touching the existing one
	result, err := m.updateCredentials(t.Context(), "test-inst", UpdateCredentialsRequest{
		Credentials: map[string]CredentialPolicy{
			"ANTHROPIC_KEY": {
				Source: CredentialSource{Env: "ANTHROPIC_KEY"},
				Inject: []CredentialInjectRule{
					{
						Hosts: []string{"api.anthropic.com"},
						As:    CredentialInjectAs{Header: "x-api-key", Format: "${value}"},
					},
				},
			},
		},
		Env: map[string]string{},
	})
	require.NoError(t, err)
	require.NotNil(t, result)

	reloaded, err := m.loadMetadata("test-inst")
	require.NoError(t, err)
	require.Len(t, reloaded.StoredMetadata.Credentials, 2)

	// Existing credential preserved
	openai, ok := reloaded.StoredMetadata.Credentials["OPENAI_KEY"]
	require.True(t, ok, "existing OPENAI_KEY credential should be preserved")
	assert.Equal(t, "OPENAI_KEY", openai.Source.Env)
	assert.Equal(t, []string{"api.openai.com"}, openai.Inject[0].Hosts)

	// New credential added
	anthropic, ok := reloaded.StoredMetadata.Credentials["ANTHROPIC_KEY"]
	require.True(t, ok, "new ANTHROPIC_KEY credential should be added")
	assert.Equal(t, "ANTHROPIC_KEY", anthropic.Source.Env)
	assert.Equal(t, []string{"api.anthropic.com"}, anthropic.Inject[0].Hosts)
}

func TestUpdateCredentials_OverridesExistingByName(t *testing.T) {
	t.Parallel()

	m := setupCredentialsTestManager(t, &metadata{
		StoredMetadata: StoredMetadata{
			Id:             "test-inst",
			Name:           "test",
			NetworkEnabled: true,
			NetworkEgress:  &NetworkEgressPolicy{Enabled: true},
			Env:            map[string]string{"MY_KEY": "old-secret"},
			Credentials: map[string]CredentialPolicy{
				"MY_KEY": {
					Source: CredentialSource{Env: "MY_KEY"},
					Inject: []CredentialInjectRule{
						{
							Hosts: []string{"api.old.com"},
							As:    CredentialInjectAs{Header: "Authorization", Format: "Bearer ${value}"},
						},
					},
				},
			},
		},
	})

	// PATCH: update existing credential with new hosts
	result, err := m.updateCredentials(t.Context(), "test-inst", UpdateCredentialsRequest{
		Credentials: map[string]CredentialPolicy{
			"MY_KEY": {
				Source: CredentialSource{Env: "MY_KEY"},
				Inject: []CredentialInjectRule{
					{
						Hosts: []string{"api.new.com"},
						As:    CredentialInjectAs{Header: "Authorization", Format: "Bearer ${value}"},
					},
				},
			},
		},
		Env: map[string]string{},
	})
	require.NoError(t, err)
	require.NotNil(t, result)

	reloaded, err := m.loadMetadata("test-inst")
	require.NoError(t, err)
	require.Len(t, reloaded.StoredMetadata.Credentials, 1)
	policy := reloaded.StoredMetadata.Credentials["MY_KEY"]
	assert.Equal(t, []string{"api.new.com"}, policy.Inject[0].Hosts, "credential should be updated with new hosts")
}

func TestUpdateCredentials_MergesEnv(t *testing.T) {
	t.Parallel()

	m := setupCredentialsTestManager(t, &metadata{
		StoredMetadata: StoredMetadata{
			Id:             "test-inst",
			Name:           "test",
			NetworkEnabled: true,
			NetworkEgress:  &NetworkEgressPolicy{Enabled: true},
			Env: map[string]string{
				"MY_KEY":       "old-secret",
				"EXISTING_VAR": "keep-me",
			},
		},
	})

	result, err := m.updateCredentials(t.Context(), "test-inst", UpdateCredentialsRequest{
		Credentials: map[string]CredentialPolicy{
			"MY_KEY": {
				Source: CredentialSource{Env: "MY_KEY"},
				Inject: []CredentialInjectRule{
					{As: CredentialInjectAs{Header: "Authorization", Format: "Bearer ${value}"}},
				},
			},
		},
		Env: map[string]string{"MY_KEY": "new-rotated-secret"},
	})
	require.NoError(t, err)
	require.NotNil(t, result)

	reloaded, err := m.loadMetadata("test-inst")
	require.NoError(t, err)
	assert.Equal(t, "new-rotated-secret", reloaded.StoredMetadata.Env["MY_KEY"])
	assert.Equal(t, "keep-me", reloaded.StoredMetadata.Env["EXISTING_VAR"])
}

func TestUpdateCredentials_NormalizesCredentials(t *testing.T) {
	t.Parallel()

	m := setupCredentialsTestManager(t, &metadata{
		StoredMetadata: StoredMetadata{
			Id:             "test-inst",
			Name:           "test",
			NetworkEnabled: true,
			NetworkEgress:  &NetworkEgressPolicy{Enabled: true},
			Env:            map[string]string{"MY_KEY": "secret"},
		},
	})

	result, err := m.updateCredentials(t.Context(), "test-inst", UpdateCredentialsRequest{
		Credentials: map[string]CredentialPolicy{
			" MY_KEY ": {
				Source: CredentialSource{Env: " MY_KEY "},
				Inject: []CredentialInjectRule{
					{
						Hosts: []string{" API.OpenAI.com "},
						As:    CredentialInjectAs{Header: " Authorization ", Format: " Bearer ${value} "},
					},
				},
			},
		},
		Env: map[string]string{},
	})
	require.NoError(t, err)
	require.NotNil(t, result)

	reloaded, err := m.loadMetadata("test-inst")
	require.NoError(t, err)
	policy, ok := reloaded.StoredMetadata.Credentials["MY_KEY"]
	require.True(t, ok)
	assert.Equal(t, "MY_KEY", policy.Source.Env)
	assert.Equal(t, "Authorization", policy.Inject[0].As.Header)
	assert.Equal(t, "Bearer ${value}", policy.Inject[0].As.Format)
	assert.Equal(t, []string{"api.openai.com"}, policy.Inject[0].Hosts)
}

func TestUpdateCredentials_RejectsInvalidFormat(t *testing.T) {
	t.Parallel()

	m := setupCredentialsTestManager(t, &metadata{
		StoredMetadata: StoredMetadata{
			Id:             "test-inst",
			Name:           "test",
			NetworkEnabled: true,
			NetworkEgress:  &NetworkEgressPolicy{Enabled: true},
			Env:            map[string]string{"MY_KEY": "secret"},
		},
	})

	_, err := m.updateCredentials(t.Context(), "test-inst", UpdateCredentialsRequest{
		Credentials: map[string]CredentialPolicy{
			"MY_KEY": {
				Source: CredentialSource{Env: "MY_KEY"},
				Inject: []CredentialInjectRule{
					{As: CredentialInjectAs{Header: "Authorization", Format: "Bearer no-template"}},
				},
			},
		},
		Env: map[string]string{},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "must include ${value}")
}

func TestUpdateCredentials_NotFoundForMissingInstance(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	m := &manager{paths: paths.New(tmpDir)}

	_, err := m.updateCredentials(t.Context(), "nonexistent", UpdateCredentialsRequest{})
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrNotFound)
}
