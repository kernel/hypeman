package instances

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestHypervisorProcessIdentityJSONKeysStayFlat guards the on-disk metadata
// format: the identity struct is embedded anonymously so its fields keep the
// flat JSON keys metadata files were written with before the struct existed.
func TestHypervisorProcessIdentityJSONKeysStayFlat(t *testing.T) {
	pid := 1234
	stored := StoredMetadata{
		Id: "inst-json",
		HypervisorProcessIdentity: HypervisorProcessIdentity{
			HypervisorPID:       &pid,
			HypervisorStartTime: 42,
			HypervisorBootID:    "boot-id",
		},
	}

	data, err := json.Marshal(stored)
	require.NoError(t, err)

	var keys map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(data, &keys))
	assert.Contains(t, keys, "HypervisorPID")
	assert.Contains(t, keys, "HypervisorStartTime")
	assert.Contains(t, keys, "HypervisorBootID")
	assert.NotContains(t, keys, "HypervisorProcessIdentity")

	var decoded StoredMetadata
	require.NoError(t, json.Unmarshal(data, &decoded))
	require.NotNil(t, decoded.HypervisorPID)
	assert.Equal(t, pid, *decoded.HypervisorPID)
	assert.Equal(t, uint64(42), decoded.HypervisorStartTime)
	assert.Equal(t, "boot-id", decoded.HypervisorBootID)
}
