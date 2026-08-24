package instances

import (
	"os/exec"
	"testing"
	"time"

	"github.com/kernel/hypeman/lib/paths"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLiveVGPUReconcileProtectionBoundsStartupProtection(t *testing.T) {
	dead := exec.Command("true")
	require.NoError(t, dead.Run())
	deadPID := dead.Process.Pid
	now := time.Now().UTC()
	recent := now.Add(-time.Minute)
	stale := now.Add(-VGPUAssignmentStartupGracePeriod - time.Minute)

	m := &manager{paths: paths.New(t.TempDir()), now: func() time.Time { return now }}
	instances := []StoredMetadata{
		{Id: "booting", GPUDevicePath: "/sys/bus/pci/devices/0000:82:00.4", GPUAssignedAt: &recent},
		{Id: "orphaned", GPUDevicePath: "/sys/bus/pci/devices/0000:82:00.5", GPUAssignedAt: &stale},
		{Id: "legacy", GPUDevicePath: "/sys/bus/pci/devices/0000:82:00.6"},
		{Id: "dead", GPUDevicePath: "/sys/bus/pci/devices/0000:82:00.7", HypervisorProcessIdentity: HypervisorProcessIdentity{HypervisorPID: &deadPID}},
		{Id: "stale-pid-booting", GPUDevicePath: "/sys/bus/pci/devices/0000:82:00.8", HypervisorProcessIdentity: HypervisorProcessIdentity{HypervisorPID: &deadPID}, GPUAssignedAt: &recent},
	}
	for i := range instances {
		require.NoError(t, m.ensureDirectories(instances[i].Id))
		require.NoError(t, m.saveMetadata(&metadata{StoredMetadata: instances[i]}))
	}

	protected, retryAfter, err := m.liveVGPUReconcileProtection(t.Context())
	require.NoError(t, err)
	assert.Equal(t, VGPUAssignmentStartupGracePeriod-time.Minute, retryAfter)
	assert.Contains(t, protected, "/sys/bus/pci/devices/0000:82:00.4")
	assert.NotContains(t, protected, "/sys/bus/pci/devices/0000:82:00.5")
	assert.NotContains(t, protected, "/sys/bus/pci/devices/0000:82:00.6")
	assert.NotContains(t, protected, "/sys/bus/pci/devices/0000:82:00.7")
	assert.Contains(t, protected, "/sys/bus/pci/devices/0000:82:00.8")
}

func TestVGPUAssignmentLiveness(t *testing.T) {
	now := time.Now().UTC()
	recent := now.Add(-time.Minute)
	stale := now.Add(-VGPUAssignmentStartupGracePeriod - time.Minute)
	pid := 123

	tests := []struct {
		name      string
		stored    StoredMetadata
		livePID   bool
		live      bool
		remaining time.Duration
	}{
		{name: "live PID", stored: StoredMetadata{HypervisorProcessIdentity: HypervisorProcessIdentity{HypervisorPID: &pid}}, livePID: true, live: true},
		{name: "dead PID recent assignment", stored: StoredMetadata{HypervisorProcessIdentity: HypervisorProcessIdentity{HypervisorPID: &pid}, GPUAssignedAt: &recent}, live: true, remaining: VGPUAssignmentStartupGracePeriod - time.Minute},
		{name: "dead PID stale assignment", stored: StoredMetadata{HypervisorProcessIdentity: HypervisorProcessIdentity{HypervisorPID: &pid}, GPUAssignedAt: &stale}},
		{name: "no PID recent assignment", stored: StoredMetadata{GPUAssignedAt: &recent}, live: true, remaining: VGPUAssignmentStartupGracePeriod - time.Minute},
		{name: "no PID stale assignment", stored: StoredMetadata{GPUAssignedAt: &stale}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			live, remaining := vgpuAssignmentLiveness(&tt.stored, now, tt.livePID)
			assert.Equal(t, tt.live, live)
			assert.Equal(t, tt.remaining, remaining)
		})
	}
}
