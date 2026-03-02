package firecracker

import (
	"testing"

	"github.com/kernel/hypeman/lib/hypervisor"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestToDriveConfigs(t *testing.T) {
	cfg := hypervisor.VMConfig{
		Disks: []hypervisor.DiskConfig{
			{Path: "/rootfs.raw", Readonly: true, IOBps: 1024, IOBurstBps: 4096},
			{Path: "/overlay.raw", Readonly: false},
		},
	}

	drives := toDriveConfigs(cfg)
	require.Len(t, drives, 2)

	assert.Equal(t, "rootfs", drives[0].DriveID)
	assert.True(t, drives[0].IsRootDevice)
	assert.True(t, drives[0].IsReadOnly)
	require.NotNil(t, drives[0].RateLimiter)
	require.NotNil(t, drives[0].RateLimiter.Bandwidth)
	assert.Equal(t, int64(1024), drives[0].RateLimiter.Bandwidth.Size)
	assert.Equal(t, int64(1000), drives[0].RateLimiter.Bandwidth.RefillTime)
	require.NotNil(t, drives[0].RateLimiter.Bandwidth.OneTimeBurst)
	assert.Equal(t, int64(3072), *drives[0].RateLimiter.Bandwidth.OneTimeBurst)

	assert.Equal(t, "disk1", drives[1].DriveID)
	assert.False(t, drives[1].IsRootDevice)
	assert.False(t, drives[1].IsReadOnly)
	assert.Nil(t, drives[1].RateLimiter)
}

func TestToNetworkInterfaces(t *testing.T) {
	cfg := hypervisor.VMConfig{
		Networks: []hypervisor.NetworkConfig{
			{
				TAPDevice:   "hype-abc123",
				MAC:         "02:00:00:00:00:01",
				DownloadBps: 1_000_000,
				UploadBps:   2_000_000,
			},
		},
	}

	nets := toNetworkInterfaces(cfg)
	require.Len(t, nets, 1)
	assert.Equal(t, "eth0", nets[0].IfaceID)
	assert.Equal(t, "hype-abc123", nets[0].HostDevName)
	assert.Equal(t, "02:00:00:00:00:01", nets[0].GuestMAC)
	require.NotNil(t, nets[0].RxRateLimiter)
	require.NotNil(t, nets[0].TxRateLimiter)
	assert.Equal(t, int64(1_000_000), nets[0].RxRateLimiter.Bandwidth.Size)
	assert.Equal(t, int64(2_000_000), nets[0].TxRateLimiter.Bandwidth.Size)
}

func TestSnapshotParamPaths(t *testing.T) {
	create := toSnapshotCreateParams("/tmp/snapshot-latest")
	assert.Equal(t, "/tmp/snapshot-latest/state", create.SnapshotPath)
	assert.Equal(t, "/tmp/snapshot-latest/memory", create.MemFilePath)
	assert.Equal(t, "Full", create.SnapshotType)

	load := toSnapshotLoadParams("/tmp/snapshot-latest", []networkOverride{
		{IfaceID: "eth0", HostDevName: "hype-abc123"},
	})
	assert.Equal(t, "/tmp/snapshot-latest/state", load.SnapshotPath)
	assert.Equal(t, "/tmp/snapshot-latest/memory", load.MemFilePath)
	assert.True(t, load.EnableDiffSnapshots)
	assert.False(t, load.ResumeVM)
	require.Len(t, load.NetworkOverrides, 1)
}
