package instances

import (
	"testing"

	"github.com/kernel/hypeman/lib/guestmemory"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPlannedVMConfig(t *testing.T) {
	t.Parallel()
	m := &manager{}

	config := m.plannedVMConfig(
		1024,
		[]VolumeAttachment{{Overlay: true}, {Overlay: false}},
		true,
		2,
	)

	assert.Equal(t, int64(1024), config.HotplugBytes)
	require.Len(t, config.Disks, 6, "three instance disks plus two overlay-volume disks plus one plain volume")
	require.Len(t, config.Networks, 1)
	require.Len(t, config.PCIDevices, 2)
	assert.Equal(t, int64(3), config.VsockCID)
}

func TestPlannedVMConfigWithoutOptionalDevices(t *testing.T) {
	t.Parallel()
	config := (&manager{}).plannedVMConfig(0, nil, false, 0)
	require.Len(t, config.Disks, baseInstanceDiskCount)
	assert.Empty(t, config.Networks)
	assert.Empty(t, config.PCIDevices)
}

func TestPlannedStoredVMConfigStandbyIgnoresLiveBalloonPolicy(t *testing.T) {
	t.Parallel()
	m := &manager{guestMemoryPolicy: guestmemory.Policy{Enabled: true, ReclaimEnabled: true}}

	stopped := m.plannedStoredVMConfig(SnapshotKindStopped, StoredMetadata{})
	assert.True(t, stopped.GuestMemory.EnableBalloon)

	standby := m.plannedStoredVMConfig(SnapshotKindStandby, StoredMetadata{})
	assert.Empty(t, standby.GuestMemory)
	assert.False(t, standby.GuestMemory.EnableBalloon)
}
