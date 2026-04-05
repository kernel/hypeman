package instances

import (
	"testing"

	"github.com/kernel/hypeman/lib/resources"
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

	first, err := NewNamedResourceLimit("^prod-.*", NamedResourceLimitConfig{
		MaxVcpusPerInstance:  &eight,
		MaxMemoryPerInstance: &sixtyFourGiB,
	})
	require.NoError(t, err)
	second, err := NewNamedResourceLimit("^prod-api-.*", NamedResourceLimitConfig{
		MaxVcpusPerInstance:  &four,
		MaxMemoryPerInstance: &thirtyTwoGiB,
		MaxOverlaySize:       &twentyGiB,
	})
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
	override, err := NewNamedResourceLimit("^small-.*", NamedResourceLimitConfig{
		MaxOverlaySize: &twentyGiB,
	})
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
	override, err := NewNamedResourceLimit("^burst-.*", NamedResourceLimitConfig{
		MaxVcpusPerInstance:  &zeroInt,
		MaxMemoryPerInstance: &zeroBytes,
		MaxOverlaySize:       &zeroBytes,
	})
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
	override, err := NewNamedResourceLimit("^db-.*", NamedResourceLimitConfig{
		MaxVcpusPerInstance: &four,
	})
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

func TestValidateProvisionedResourceLimitsForName_RejectsProjectedTotal(t *testing.T) {
	t.Parallel()

	eight := 8
	oneHundredTwentyEightGiB := int64(128 * 1024 * 1024 * 1024)
	oneTiB := int64(1024 * 1024 * 1024 * 1024)
	twoGbps := int64(2 * 1000 * 1000 * 1000 / 8)
	oneGiBps := int64(1024 * 1024 * 1024)

	pattern, err := NewNamedResourceLimit("^team-a-", NamedResourceLimitConfig{
		MaxTotalVcpus:            &eight,
		MaxTotalMemory:           &oneHundredTwentyEightGiB,
		MaxTotalDisk:             &oneTiB,
		MaxTotalNetworkBandwidth: &twoGbps,
		MaxTotalDiskIO:           &oneGiBps,
	})
	require.NoError(t, err)

	limits := ResourceLimits{NamePatterns: []NamedResourceLimit{pattern}}
	existing := []resources.InstanceAllocation{
		{
			Name:               "team-a-api-1",
			Vcpus:              6,
			MemoryBytes:        96 * 1024 * 1024 * 1024,
			OverlayBytes:       300 * 1024 * 1024 * 1024,
			VolumeBytes:        500 * 1024 * 1024 * 1024,
			VolumeOverlayBytes: 50 * 1024 * 1024 * 1024,
			NetworkDownloadBps: 100 * 1024 * 1024,
			NetworkUploadBps:   200 * 1024 * 1024,
			DiskIOBps:          400 * 1024 * 1024,
		},
	}

	err = validateProvisionedResourceLimitsForName("team-a-worker-1", limits, existing, provisionedResources{
		Vcpus:       4,
		MemoryBytes: 40 * 1024 * 1024 * 1024,
		DiskBytes:   300 * 1024 * 1024 * 1024,
		NetworkBps:  100 * 1024 * 1024,
		DiskIOBps:   200 * 1024 * 1024,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "total provisioned cpu 10")
}

func TestValidateProvisionedResourceLimitsForName_UsesFirstMatchingPattern(t *testing.T) {
	t.Parallel()

	four := 4
	eight := 8
	first, err := NewNamedResourceLimit("^prod-", NamedResourceLimitConfig{
		MaxTotalVcpus: &four,
	})
	require.NoError(t, err)
	second, err := NewNamedResourceLimit("^prod-api-", NamedResourceLimitConfig{
		MaxTotalVcpus: &eight,
	})
	require.NoError(t, err)

	limits := ResourceLimits{NamePatterns: []NamedResourceLimit{first, second}}
	existing := []resources.InstanceAllocation{
		{Name: "prod-api-1", Vcpus: 3},
	}

	err = validateProvisionedResourceLimitsForName("prod-api-2", limits, existing, provisionedResources{Vcpus: 2})
	require.Error(t, err)
	assert.Contains(t, err.Error(), `pattern "^prod-"`)
}
