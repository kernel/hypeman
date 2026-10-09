package instances

import (
	"testing"

	"github.com/stretchr/testify/require"
)

type proportionalShapingValidator struct{ recordingResourceValidator }

func (*proportionalShapingValidator) DefaultNetworkBandwidth(vcpus int) (int64, int64) {
	return int64(vcpus) * 10, int64(vcpus) * 20
}

func (*proportionalShapingValidator) DefaultDiskIOBandwidth(vcpus int) (int64, int64) {
	return int64(vcpus) * 100, 0
}

func TestApplyLinuxShapingDefaultsFillsOnlyUnspecifiedLimits(t *testing.T) {
	t.Parallel()
	m := &manager{resourceValidator: &proportionalShapingValidator{}}

	req := CreateInstanceRequest{NetworkBandwidthUpload: 7}
	m.applyLinuxShapingDefaults(&req, 2)
	require.Equal(t, int64(200), req.DiskIOBps)
	require.Equal(t, int64(20), req.NetworkBandwidthDownload)
	require.Equal(t, int64(7), req.NetworkBandwidthUpload)

	req = CreateInstanceRequest{DiskIOBps: 5}
	m.applyLinuxShapingDefaults(&req, 4)
	require.Equal(t, int64(5), req.DiskIOBps)
	require.Equal(t, int64(40), req.NetworkBandwidthDownload)
	require.Equal(t, int64(80), req.NetworkBandwidthUpload)
}

func TestApplyLinuxShapingDefaultsWithoutValidatorLeavesAuto(t *testing.T) {
	t.Parallel()
	m := &manager{}

	req := CreateInstanceRequest{}
	m.applyLinuxShapingDefaults(&req, 2)
	require.Zero(t, req.DiskIOBps)
	require.Zero(t, req.NetworkBandwidthDownload)
	require.Zero(t, req.NetworkBandwidthUpload)
}
