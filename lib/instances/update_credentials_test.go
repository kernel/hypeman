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

func TestUpdateCredentials_ClearsCredentials(t *testing.T) {
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

	result, err := m.updateCredentials(t.Context(), "test-inst", UpdateCredentialsRequest{
		Credentials: nil,
		Env:         map[string]string{},
	})
	require.NoError(t, err)
	require.NotNil(t, result)

	reloaded, err := m.loadMetadata("test-inst")
	require.NoError(t, err)
	assert.Empty(t, reloaded.StoredMetadata.Credentials)
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
