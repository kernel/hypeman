package instances

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestValidateUpdateInstanceRequest(t *testing.T) {
	t.Parallel()

	baseMeta := &metadata{
		StoredMetadata: StoredMetadata{
			NetworkEgress: &NetworkEgressPolicy{Enabled: true},
			Credentials: map[string]CredentialPolicy{
				"OUTBOUND_OPENAI_KEY": {
					Source: CredentialSource{Env: "OUTBOUND_OPENAI_KEY"},
				},
			},
		},
	}

	t.Run("requires at least one env key", func(t *testing.T) {
		t.Parallel()
		err := validateUpdateInstanceRequest(baseMeta, UpdateInstanceRequest{})
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrInvalidRequest)
		assert.Contains(t, err.Error(), "at least one credential source env var")
	})

	t.Run("rejects instances without credential backed envs", func(t *testing.T) {
		t.Parallel()
		err := validateUpdateInstanceRequest(&metadata{}, UpdateInstanceRequest{
			Env: map[string]string{"OUTBOUND_OPENAI_KEY": "rotated"},
		})
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrInvalidRequest)
		assert.Contains(t, err.Error(), "no credential-backed env vars")
	})

	t.Run("rejects unrelated env keys", func(t *testing.T) {
		t.Parallel()
		err := validateUpdateInstanceRequest(baseMeta, UpdateInstanceRequest{
			Env: map[string]string{"UNRELATED_KEY": "value"},
		})
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrInvalidRequest)
		assert.Contains(t, err.Error(), "UNRELATED_KEY")
		assert.Contains(t, err.Error(), "OUTBOUND_OPENAI_KEY")
	})

	t.Run("allows credential source env keys", func(t *testing.T) {
		t.Parallel()
		err := validateUpdateInstanceRequest(baseMeta, UpdateInstanceRequest{
			Env: map[string]string{"OUTBOUND_OPENAI_KEY": "rotated"},
		})
		require.NoError(t, err)
	})
}
