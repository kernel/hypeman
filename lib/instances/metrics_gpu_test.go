package instances

import (
	"testing"

	"github.com/kernel/hypeman/lib/devices"
	"github.com/stretchr/testify/assert"
)

func TestCountGPUMdevs(t *testing.T) {
	t.Parallel()

	mdevs := []devices.MdevDevice{
		{UUID: "claimed-running"},
		{UUID: "claimed-stopped"},
		{UUID: "orphaned-a"},
		{UUID: "orphaned-b"},
	}
	instances := []Instance{
		{StoredMetadata: StoredMetadata{GPUMdevUUID: "claimed-running"}, State: StateRunning},
		{StoredMetadata: StoredMetadata{GPUMdevUUID: "claimed-stopped"}, State: StateStopped},
		{StoredMetadata: StoredMetadata{GPUMdevUUID: "no-longer-on-host"}, State: StateStopped},
		{State: StateRunning},
	}

	claimed, orphaned := countGPUMdevs(mdevs, instances)
	assert.Equal(t, int64(2), claimed)
	assert.Equal(t, int64(2), orphaned)

	claimed, orphaned = countGPUMdevs(nil, instances)
	assert.Equal(t, int64(0), claimed)
	assert.Equal(t, int64(0), orphaned)

	claimed, orphaned = countGPUMdevs(mdevs, nil)
	assert.Equal(t, int64(0), claimed)
	assert.Equal(t, int64(4), orphaned)
}
