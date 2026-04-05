package instances

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestResourceLimitsForName_FirstMatchWins(t *testing.T) {
	t.Parallel()

	eight := 8
	four := 4
	sixtyFourGiB := int64(64 * 1024 * 1024 * 1024)
	thirtyTwoGiB := int64(32 * 1024 * 1024 * 1024)
	twentyGiB := int64(20 * 1024 * 1024 * 1024)

	first, err := NewNamedResourceLimit("^prod-.*", &eight, &sixtyFourGiB, nil)
	require.NoError(t, err)
	second, err := NewNamedResourceLimit("^prod-api-.*", &four, &thirtyTwoGiB, &twentyGiB)
	require.NoError(t, err)

	limits := ResourceLimits{
		MaxOverlaySize:       10 * 1024 * 1024 * 1024,
		MaxVcpusPerInstance:  2,
		MaxMemoryPerInstance: 8 * 1024 * 1024 * 1024,
		NamePatterns:         []NamedResourceLimit{first, second},
	}

	resolved := limits.ForName("prod-api-1")
	assert.Equal(t, 8, resolved.MaxVcpusPerInstance)
	assert.Equal(t, sixtyFourGiB, resolved.MaxMemoryPerInstance)
	assert.Equal(t, int64(10*1024*1024*1024), resolved.MaxOverlaySize)
}

func TestResourceLimitsForName_FallsBackWhenFieldOmitted(t *testing.T) {
	t.Parallel()

	twentyGiB := int64(20 * 1024 * 1024 * 1024)
	override, err := NewNamedResourceLimit("^small-.*", nil, nil, &twentyGiB)
	require.NoError(t, err)

	limits := ResourceLimits{
		MaxOverlaySize:       100 * 1024 * 1024 * 1024,
		MaxVcpusPerInstance:  16,
		MaxMemoryPerInstance: 32 * 1024 * 1024 * 1024,
		NamePatterns:         []NamedResourceLimit{override},
	}

	resolved := limits.ForName("small-worker")
	assert.Equal(t, int64(20*1024*1024*1024), resolved.MaxOverlaySize)
	assert.Equal(t, 16, resolved.MaxVcpusPerInstance)
	assert.Equal(t, int64(32*1024*1024*1024), resolved.MaxMemoryPerInstance)
}

func TestValidateResourceLimitsForName_ZeroOverrideMeansUnlimited(t *testing.T) {
	t.Parallel()

	zeroInt := 0
	zeroBytes := int64(0)
	override, err := NewNamedResourceLimit("^burst-.*", &zeroInt, &zeroBytes, &zeroBytes)
	require.NoError(t, err)

	limits := ResourceLimits{
		MaxOverlaySize:       5 * 1024 * 1024 * 1024,
		MaxVcpusPerInstance:  2,
		MaxMemoryPerInstance: 4 * 1024 * 1024 * 1024,
		NamePatterns:         []NamedResourceLimit{override},
	}

	err = validateResourceLimitsForName("burst-worker", limits, 50*1024*1024*1024, 32, 128*1024*1024*1024)
	require.NoError(t, err)
}

func TestValidateResourceLimitsForName_RejectsWhenResolvedLimitExceeded(t *testing.T) {
	t.Parallel()

	four := 4
	override, err := NewNamedResourceLimit("^db-.*", &four, nil, nil)
	require.NoError(t, err)

	limits := ResourceLimits{
		MaxOverlaySize:       100 * 1024 * 1024 * 1024,
		MaxVcpusPerInstance:  16,
		MaxMemoryPerInstance: 64 * 1024 * 1024 * 1024,
		NamePatterns:         []NamedResourceLimit{override},
	}

	err = validateResourceLimitsForName("db-primary", limits, 10*1024*1024*1024, 8, 16*1024*1024*1024)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "vcpus 8 exceeds maximum allowed 4 per instance")
}
