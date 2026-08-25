package instances

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kernel/hypeman/lib/devices"
	"github.com/kernel/hypeman/lib/paths"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func waitForOrphanQueueEmpty(t *testing.T, m *manager) {
	t.Helper()
	require.Eventually(t, func() bool {
		m.orphanedVGPUMu.Lock()
		defer m.orphanedVGPUMu.Unlock()
		return len(m.orphanedVGPUs) == 0
	}, 5*time.Second, 5*time.Millisecond, "orphan retry should finish and clear its queue entry")
}

func TestScheduleOrphanedVGPUReleaseRetriesUntilSuccess(t *testing.T) {
	t.Parallel()

	var attempts atomic.Int32
	m := &manager{
		paths:                  paths.New(t.TempDir()),
		orphanedVGPURetryDelay: time.Millisecond,
		destroyVGPU: func(context.Context, devices.VGPUAssignment) error {
			if attempts.Add(1) < 3 {
				return errors.New("operation not permitted")
			}
			return nil
		},
	}
	m.scheduleOrphanedVGPURelease(context.Background(), StoredMetadata{
		Id:            "deleted-instance",
		GPUFramework:  devices.VGPUFrameworkVendorVFIO,
		GPUDevicePath: "/sys/bus/pci/devices/0000:82:00.4",
	})

	waitForOrphanQueueEmpty(t, m)
	assert.Equal(t, int32(3), attempts.Load(), "release should succeed on the third attempt and stop retrying")
}

func TestScheduleOrphanedVGPUReleaseGivesUpAfterMaxAttempts(t *testing.T) {
	t.Parallel()

	var attempts atomic.Int32
	m := &manager{
		paths:                  paths.New(t.TempDir()),
		orphanedVGPURetryDelay: time.Millisecond,
		destroyVGPU: func(context.Context, devices.VGPUAssignment) error {
			attempts.Add(1)
			return errors.New("vGPU destroy failed: 0xffffffff")
		},
	}
	m.scheduleOrphanedVGPURelease(context.Background(), StoredMetadata{
		Id:            "deleted-instance",
		GPUFramework:  devices.VGPUFrameworkVendorVFIO,
		GPUDevicePath: "/sys/bus/pci/devices/0000:82:00.4",
	})

	waitForOrphanQueueEmpty(t, m)
	assert.Equal(t, int32(orphanedVGPUReleaseMaxAttempts), attempts.Load(),
		"a wedged VF should get exactly the bounded number of attempts")
}

func TestScheduleOrphanedVGPUReleaseDeduplicatesByDevicePath(t *testing.T) {
	t.Parallel()

	var attempts atomic.Int32
	release := make(chan struct{})
	m := &manager{
		paths:                  paths.New(t.TempDir()),
		orphanedVGPURetryDelay: 20 * time.Millisecond,
		destroyVGPU: func(context.Context, devices.VGPUAssignment) error {
			attempts.Add(1)
			<-release
			return nil
		},
	}
	stored := StoredMetadata{
		Id:            "deleted-instance",
		GPUFramework:  devices.VGPUFrameworkVendorVFIO,
		GPUDevicePath: "/sys/bus/pci/devices/0000:82:00.4",
	}
	m.scheduleOrphanedVGPURelease(context.Background(), stored)
	m.scheduleOrphanedVGPURelease(context.Background(), stored)

	require.Eventually(t, func() bool { return attempts.Load() == 1 }, 5*time.Second, 5*time.Millisecond)
	close(release)
	waitForOrphanQueueEmpty(t, m)
	assert.Equal(t, int32(1), attempts.Load(), "the second schedule for the same path must be dropped")
}

func TestOrphanedVGPUReleaseReappliesClaimScan(t *testing.T) {
	t.Parallel()

	var destroys atomic.Int32
	m := &manager{
		paths:                  paths.New(t.TempDir()),
		orphanedVGPURetryDelay: time.Millisecond,
		destroyVGPU: func(context.Context, devices.VGPUAssignment) error {
			destroys.Add(1)
			return nil
		},
	}
	require.NoError(t, m.ensureDirectories("mid-boot-claimant"))
	assignedAt := time.Now()
	require.NoError(t, m.saveMetadata(&metadata{StoredMetadata: StoredMetadata{
		Id:            "mid-boot-claimant",
		Name:          "mid-boot-claimant",
		GPUFramework:  devices.VGPUFrameworkVendorVFIO,
		GPUDevicePath: "/sys/bus/pci/devices/0000:82:00.4",
		GPUAssignedAt: &assignedAt,
	}}))

	m.scheduleOrphanedVGPURelease(context.Background(), StoredMetadata{
		Id:            "deleted-instance",
		GPUFramework:  devices.VGPUFrameworkVendorVFIO,
		GPUDevicePath: "/sys/bus/pci/devices/0000:82:00.4",
	})

	waitForOrphanQueueEmpty(t, m)
	assert.Zero(t, destroys.Load(), "no destroy may fire while the claim scan cannot clear the path")
}
