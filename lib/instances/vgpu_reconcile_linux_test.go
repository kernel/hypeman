//go:build linux

package instances

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kernel/hypeman/lib/devices"
	"github.com/kernel/hypeman/lib/paths"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	otelmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

func TestReconcileVGPUsProtectsSocketOwnerWithoutPersistedPID(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "test.sock")
	listener, err := net.Listen("unix", socketPath)
	require.NoError(t, err)
	defer listener.Close()

	var destroyed []devices.VGPUAssignment
	var protected map[string]struct{}
	m := &manager{
		paths: paths.New(t.TempDir()),
		destroyVGPU: func(_ context.Context, assignment devices.VGPUAssignment) error {
			destroyed = append(destroyed, assignment)
			return nil
		},
		reconcileVGPUDevices: func(_ context.Context, p map[string]struct{}) error {
			protected = p
			return nil
		},
	}
	const id = "pid-save-failed"
	require.NoError(t, m.ensureDirectories(id))
	require.NoError(t, m.saveMetadata(&metadata{StoredMetadata: StoredMetadata{
		Id:            id,
		SocketPath:    socketPath,
		GPUFramework:  devices.VGPUFrameworkMdev,
		GPUDevicePath: "/sys/bus/mdev/devices/test-mdev",
		GPUMdevUUID:   "test-mdev",
	}}))

	m.ReconcileVGPUs(t.Context())

	assert.Empty(t, destroyed)
	assert.Contains(t, protected, "/sys/bus/mdev/devices/test-mdev")
	stored, err := m.loadMetadata(id)
	require.NoError(t, err)
	assert.Equal(t, "/sys/bus/mdev/devices/test-mdev", stored.GPUDevicePath)
}

func TestStartVGPUReconcilerResetsUnclaimedVendorVFs(t *testing.T) {
	vfs := []devices.VirtualFunction{
		{PCIAddress: "0000:82:00.4", ParentGPU: "0000:82:00.0", Allocated: true, ProfileType: testVFProfileType},
		{PCIAddress: "0000:82:00.5", ParentGPU: "0000:82:00.0", Allocated: true, ProfileType: testVFProfileType},
		{PCIAddress: "0000:82:00.6", ParentGPU: "0000:82:00.0"},
	}
	var destroyed []string
	m := &manager{
		paths: paths.New(t.TempDir()),
		discoverVGPU: func() (devices.VGPUFramework, []devices.VirtualFunction, error) {
			return devices.VGPUFrameworkVendorVFIO, vfs, nil
		},
		destroyVGPU: func(_ context.Context, assignment devices.VGPUAssignment) error {
			destroyed = append(destroyed, assignment.DevicePath)
			return nil
		},
		reconcileVGPUDevices:  func(context.Context, map[string]struct{}) error { return nil },
		refreshVGPUCapacity:   func() {},
		vgpuReconcileInterval: time.Hour,
	}
	liveIdentity := HypervisorProcessIdentity{}
	liveIdentity.Set(os.Getpid())
	owner := saveTestVGPUInstance(t, m, "owner")
	owner.GPUFramework = devices.VGPUFrameworkVendorVFIO
	owner.GPUDevicePath = testVFDevicePath
	owner.HypervisorProcessIdentity = liveIdentity
	require.NoError(t, m.saveMetadata(owner))

	m.StartVGPUReconciler(t.Context())
	assert.Equal(t, []string{"/sys/bus/pci/devices/0000:82:00.5"}, destroyed,
		"only the configured VF no instance claims is reset")
}

func TestReconcileVGPUsRetriesRefusedUnclaimedVFReset(t *testing.T) {
	reader := otelmetric.NewManualReader()
	provider := otelmetric.NewMeterProvider(otelmetric.WithReader(reader))
	inUse := true
	var destroyed []string
	refreshes := 0
	m := &manager{
		paths: paths.New(t.TempDir()),
		discoverVGPU: func() (devices.VGPUFramework, []devices.VirtualFunction, error) {
			return devices.VGPUFrameworkVendorVFIO, []devices.VirtualFunction{
				{PCIAddress: "0000:82:00.4", ParentGPU: "0000:82:00.0", Allocated: true, ProfileType: testVFProfileType},
			}, nil
		},
		destroyVGPU: func(_ context.Context, assignment devices.VGPUAssignment) error {
			if inUse {
				return errors.New("vendor VFIO vGPU on VF 0000:82:00.4 is still in use")
			}
			destroyed = append(destroyed, assignment.DevicePath)
			return nil
		},
		reconcileVGPUDevices: func(context.Context, map[string]struct{}) error { return nil },
		refreshVGPUCapacity:  func() { refreshes++ },
	}
	metrics, err := newInstanceMetrics(provider.Meter("test"), nil, m)
	require.NoError(t, err)
	m.metrics = metrics

	m.ReconcileVGPUs(t.Context())
	assert.Empty(t, destroyed)
	var rm metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(t.Context(), &rm))
	failures, ok := findMetric(t, rm, "hypeman_instances_vgpu_reconcile_failures_total").Data.(metricdata.Sum[int64])
	require.True(t, ok)
	require.Len(t, failures.DataPoints, 1)
	assert.Equal(t, "reset_unclaimed_vfs", metricLabel(t, failures.DataPoints[0].Attributes, "stage"))

	inUse = false
	m.ReconcileVGPUs(t.Context())
	assert.Equal(t, []string{"/sys/bus/pci/devices/0000:82:00.4"}, destroyed, "the next pass retries the reset")
	assert.Equal(t, 2, refreshes, "every pass reloads the placements")
}
