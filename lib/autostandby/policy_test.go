package autostandby

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNormalizePolicyCanonicalizesValues(t *testing.T) {
	t.Parallel()

	normalized, err := NormalizePolicy(&Policy{
		Enabled:                true,
		IdleTimeout:            "300s",
		IgnoreSourceCIDRs:      []string{"10.0.0.0/8", " 10.0.0.0/8 ", "192.168.1.1/24"},
		IgnoreDestinationPorts: []uint16{9000, 22, 9000},
	})
	require.NoError(t, err)
	require.NotNil(t, normalized)

	assert.True(t, normalized.Enabled)
	assert.Equal(t, "5m0s", normalized.IdleTimeout)
	assert.Equal(t, []string{"10.0.0.0/8", "192.168.1.0/24"}, normalized.IgnoreSourceCIDRs)
	assert.Equal(t, []uint16{22, 9000}, normalized.IgnoreDestinationPorts)
}

func TestNormalizePolicyRejectsInvalidValues(t *testing.T) {
	t.Parallel()

	_, err := NormalizePolicy(&Policy{
		Enabled:     true,
		IdleTimeout: "0s",
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "must be positive")

	_, err = NormalizePolicy(&Policy{
		Enabled:                true,
		IdleTimeout:            "5m",
		IgnoreDestinationPorts: []uint16{0},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "must not contain 0")
}
