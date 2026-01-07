package instances

import (
	"context"
	"os"
	"syscall"
	"testing"
	"time"

	"github.com/onkernel/hypeman/cmd/api/config"
	"github.com/onkernel/hypeman/lib/devices"
	"github.com/onkernel/hypeman/lib/images"
	"github.com/onkernel/hypeman/lib/network"
	"github.com/onkernel/hypeman/lib/paths"
	"github.com/onkernel/hypeman/lib/system"
	"github.com/onkernel/hypeman/lib/volumes"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestValidateVolumeAttachments_MaxVolumes(t *testing.T) {
	// Create 24 volumes (exceeds limit of 23)
	volumes := make([]VolumeAttachment, 24)
	for i := range volumes {
		volumes[i] = VolumeAttachment{
			VolumeID:  "vol-" + string(rune('a'+i)),
			MountPath: "/mnt/vol" + string(rune('a'+i)),
		}
	}

	err := validateVolumeAttachments(volumes)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "cannot attach more than 23")
}

func TestValidateVolumeAttachments_SystemDirectory(t *testing.T) {
	volumes := []VolumeAttachment{{
		VolumeID:  "vol-1",
		MountPath: "/etc/secrets", // system directory
	}}

	err := validateVolumeAttachments(volumes)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "system directory")
}

func TestValidateVolumeAttachments_DuplicatePaths(t *testing.T) {
	volumes := []VolumeAttachment{
		{VolumeID: "vol-1", MountPath: "/mnt/data"},
		{VolumeID: "vol-2", MountPath: "/mnt/data"}, // duplicate
	}

	err := validateVolumeAttachments(volumes)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "duplicate mount path")
}

func TestValidateVolumeAttachments_RelativePath(t *testing.T) {
	volumes := []VolumeAttachment{{
		VolumeID:  "vol-1",
		MountPath: "relative/path", // not absolute
	}}

	err := validateVolumeAttachments(volumes)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "must be absolute")
}

func TestValidateVolumeAttachments_Valid(t *testing.T) {
	volumes := []VolumeAttachment{
		{VolumeID: "vol-1", MountPath: "/mnt/data"},
		{VolumeID: "vol-2", MountPath: "/mnt/logs"},
	}

	err := validateVolumeAttachments(volumes)
	assert.NoError(t, err)
}

func TestValidateVolumeAttachments_Empty(t *testing.T) {
	err := validateVolumeAttachments(nil)
	assert.NoError(t, err)

	err = validateVolumeAttachments([]VolumeAttachment{})
	assert.NoError(t, err)
}

func TestValidateVolumeAttachments_OverlayRequiresReadonly(t *testing.T) {
	// Overlay=true with Readonly=false should fail
	volumes := []VolumeAttachment{{
		VolumeID:    "vol-1",
		MountPath:   "/mnt/data",
		Readonly:    false, // Invalid: overlay requires readonly=true
		Overlay:     true,
		OverlaySize: 100 * 1024 * 1024,
	}}

	err := validateVolumeAttachments(volumes)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "overlay mode requires readonly=true")
}

func TestValidateVolumeAttachments_OverlayRequiresSize(t *testing.T) {
	// Overlay=true without OverlaySize should fail
	volumes := []VolumeAttachment{{
		VolumeID:    "vol-1",
		MountPath:   "/mnt/data",
		Readonly:    true,
		Overlay:     true,
		OverlaySize: 0, // Invalid: overlay requires size
	}}

	err := validateVolumeAttachments(volumes)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "overlay_size is required")
}

func TestValidateVolumeAttachments_OverlayValid(t *testing.T) {
	// Valid overlay configuration
	volumes := []VolumeAttachment{{
		VolumeID:    "vol-1",
		MountPath:   "/mnt/data",
		Readonly:    true,
		Overlay:     true,
		OverlaySize: 100 * 1024 * 1024, // 100MB
	}}

	err := validateVolumeAttachments(volumes)
	assert.NoError(t, err)
}

func TestValidateVolumeAttachments_OverlayCountsAsTwoDevices(t *testing.T) {
	// 12 regular volumes + 12 overlay volumes = 12 + 24 = 36 devices (exceeds 23)
	// But let's be more precise: 11 overlay volumes = 22 devices, + 1 regular = 23 (at limit)
	// 12 overlay volumes = 24 devices (exceeds limit)
	volumes := make([]VolumeAttachment, 12)
	for i := range volumes {
		volumes[i] = VolumeAttachment{
			VolumeID:    "vol-" + string(rune('a'+i)),
			MountPath:   "/mnt/vol" + string(rune('a'+i)),
			Readonly:    true,
			Overlay:     true,
			OverlaySize: 100 * 1024 * 1024,
		}
	}

	err := validateVolumeAttachments(volumes)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "cannot attach more than 23")
}

// createTestManager creates a manager with specified limits for testing
func createTestManager(t *testing.T, limits ResourceLimits) *manager {
	t.Helper()
	tmpDir := t.TempDir()
	cfg := &config.Config{DataDir: tmpDir}
	p := paths.New(cfg.DataDir)

	imageMgr, err := images.NewManager(p, 1, nil)
	require.NoError(t, err)

	systemMgr := system.NewManager(p)
	networkMgr := network.NewManager(p, cfg, nil)
	deviceMgr := devices.NewManager(p)
	volumeMgr := volumes.NewManager(p, 0, nil)

	return NewManager(p, imageMgr, systemMgr, networkMgr, deviceMgr, volumeMgr, limits, "", nil, nil).(*manager)
}

func TestResourceLimits_StructValues(t *testing.T) {
	limits := ResourceLimits{
		MaxOverlaySize:       10 * 1024 * 1024 * 1024, // 10GB
		MaxVcpusPerInstance:  4,
		MaxMemoryPerInstance: 8 * 1024 * 1024 * 1024, // 8GB
		MaxTotalVcpus:        16,
		MaxTotalMemory:       32 * 1024 * 1024 * 1024, // 32GB
	}

	assert.Equal(t, int64(10*1024*1024*1024), limits.MaxOverlaySize)
	assert.Equal(t, 4, limits.MaxVcpusPerInstance)
	assert.Equal(t, int64(8*1024*1024*1024), limits.MaxMemoryPerInstance)
	assert.Equal(t, 16, limits.MaxTotalVcpus)
	assert.Equal(t, int64(32*1024*1024*1024), limits.MaxTotalMemory)
}

func TestResourceLimits_ZeroMeansUnlimited(t *testing.T) {
	// Zero values should mean unlimited
	limits := ResourceLimits{
		MaxOverlaySize:       100 * 1024 * 1024 * 1024,
		MaxVcpusPerInstance:  0, // unlimited
		MaxMemoryPerInstance: 0, // unlimited
		MaxTotalVcpus:        0, // unlimited
		MaxTotalMemory:       0, // unlimited
	}

	mgr := createTestManager(t, limits)

	// With zero limits, manager should be created successfully
	assert.NotNil(t, mgr)
	assert.Equal(t, 0, mgr.limits.MaxVcpusPerInstance)
	assert.Equal(t, int64(0), mgr.limits.MaxMemoryPerInstance)
}

func TestAggregateUsage_NoInstances(t *testing.T) {
	limits := ResourceLimits{
		MaxOverlaySize: 100 * 1024 * 1024 * 1024,
	}
	mgr := createTestManager(t, limits)

	usage, err := mgr.calculateAggregateUsage(context.Background())
	require.NoError(t, err)

	assert.Equal(t, 0, usage.TotalVcpus)
	assert.Equal(t, int64(0), usage.TotalMemory)
}

func TestAggregateUsage_StructValues(t *testing.T) {
	usage := AggregateUsage{
		TotalVcpus:  8,
		TotalMemory: 16 * 1024 * 1024 * 1024,
	}

	assert.Equal(t, 8, usage.TotalVcpus)
	assert.Equal(t, int64(16*1024*1024*1024), usage.TotalMemory)
}

// TestAggregateLimits_EnforcedAtRuntime is an integration test that verifies
// aggregate resource limits are enforced when creating VMs.
// It creates one VM, then tries to create another that would exceed the total limit.
func TestAggregateLimits_EnforcedAtRuntime(t *testing.T) {
	// Skip in short mode - this is an integration test
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	// Require KVM access
	if _, err := os.Stat("/dev/kvm"); os.IsNotExist(err) {
		t.Skip("/dev/kvm not available - skipping VM test")
	}

	ctx := context.Background()
	tmpDir := t.TempDir()

	cfg := &config.Config{
		DataDir:    tmpDir,
		BridgeName: "vmbr0",
		SubnetCIDR: "10.100.0.0/16",
		DNSServer:  "1.1.1.1",
	}

	p := paths.New(tmpDir)
	imageManager, err := images.NewManager(p, 1, nil)
	require.NoError(t, err)

	systemManager := system.NewManager(p)
	networkManager := network.NewManager(p, cfg, nil)
	deviceManager := devices.NewManager(p)
	volumeManager := volumes.NewManager(p, 0, nil)

	// Set small aggregate limits:
	// - MaxTotalVcpus: 2 (first VM gets 1, second wants 2 -> denied)
	// - MaxTotalMemory: 6GB (first VM gets 2.5GB, second wants 4GB -> denied)
	limits := ResourceLimits{
		MaxOverlaySize:       100 * 1024 * 1024 * 1024, // 100GB
		MaxVcpusPerInstance:  4,                        // per-instance limit (high)
		MaxMemoryPerInstance: 8 * 1024 * 1024 * 1024,   // 8GB per-instance (high)
		MaxTotalVcpus:        2,                        // aggregate: only 2 total
		MaxTotalMemory:       6 * 1024 * 1024 * 1024,   // aggregate: only 6GB total (allows first 2.5GB VM)
	}

	mgr := NewManager(p, imageManager, systemManager, networkManager, deviceManager, volumeManager, limits, "", nil, nil).(*manager)

	// Cleanup any orphaned processes on test end
	t.Cleanup(func() {
		cleanupTestProcesses(t, mgr)
	})

	// Pull a small image
	t.Log("Pulling alpine image...")
	alpineImage, err := imageManager.CreateImage(ctx, images.CreateImageRequest{
		Name: "docker.io/library/alpine:latest",
	})
	require.NoError(t, err)

	// Wait for image to be ready
	t.Log("Waiting for image build...")
	for i := 0; i < 120; i++ {
		img, err := imageManager.GetImage(ctx, alpineImage.Name)
		if err == nil && img.Status == images.StatusReady {
			break
		}
		if err == nil && img.Status == images.StatusFailed {
			t.Fatalf("Image build failed: %s", *img.Error)
		}
		time.Sleep(1 * time.Second)
	}
	t.Log("Image ready")

	// Ensure system files
	t.Log("Ensuring system files...")
	err = systemManager.EnsureSystemFiles(ctx)
	require.NoError(t, err)

	// Check initial aggregate usage (should be 0)
	usage, err := mgr.calculateAggregateUsage(ctx)
	require.NoError(t, err)
	assert.Equal(t, 0, usage.TotalVcpus, "Initial vCPUs should be 0")
	assert.Equal(t, int64(0), usage.TotalMemory, "Initial memory should be 0")

	// Create first VM: 1 vCPU, 2GB + 512MB = 2.5GB memory
	t.Log("Creating first instance (1 vCPU, 2.5GB memory)...")
	inst1, err := mgr.CreateInstance(ctx, CreateInstanceRequest{
		Name:           "small-vm-1",
		Image:          "docker.io/library/alpine:latest",
		Vcpus:          1,
		Size:           2 * 1024 * 1024 * 1024, // 2GB (needs extra room for initrd with NVIDIA libs)
		HotplugSize:    512 * 1024 * 1024,      // 512MB
		OverlaySize:    1 * 1024 * 1024 * 1024,
		NetworkEnabled: false,
	})
	require.NoError(t, err)
	require.NotNil(t, inst1)
	t.Logf("First instance created: %s", inst1.Id)

	// Verify aggregate usage increased
	usage, err = mgr.calculateAggregateUsage(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, usage.TotalVcpus, "Should have 1 vCPU in use")
	assert.Equal(t, int64(2*1024*1024*1024+512*1024*1024), usage.TotalMemory, "Should have 2.5GB memory in use")
	t.Logf("Aggregate usage after first VM: %d vCPUs, %d bytes memory", usage.TotalVcpus, usage.TotalMemory)

	// Try to create second VM: 2 vCPUs (would exceed MaxTotalVcpus=2)
	// Note: 2 vCPUs alone is fine (under MaxVcpusPerInstance=4), but 1+2=3 exceeds MaxTotalVcpus=2
	t.Log("Attempting to create second instance (2 vCPUs) - should be denied...")
	_, err = mgr.CreateInstance(ctx, CreateInstanceRequest{
		Name:           "big-vm-2",
		Image:          "docker.io/library/alpine:latest",
		Vcpus:          2, // This would make total 3, exceeding limit of 2
		Size:           256 * 1024 * 1024,
		HotplugSize:    256 * 1024 * 1024,
		OverlaySize:    1 * 1024 * 1024 * 1024,
		NetworkEnabled: false,
	})
	require.Error(t, err, "Should deny creation due to aggregate vCPU limit")
	assert.Contains(t, err.Error(), "exceeds aggregate limit")
	t.Logf("Second instance correctly denied: %v", err)

	// Verify aggregate usage didn't change (failed creation shouldn't affect it)
	usage, err = mgr.calculateAggregateUsage(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, usage.TotalVcpus, "vCPUs should still be 1")

	// Clean up first instance
	t.Log("Deleting first instance...")
	err = mgr.DeleteInstance(ctx, inst1.Id)
	require.NoError(t, err)

	// Verify aggregate usage is back to 0
	usage, err = mgr.calculateAggregateUsage(ctx)
	require.NoError(t, err)
	assert.Equal(t, 0, usage.TotalVcpus, "vCPUs should be 0 after deletion")
	t.Log("Test passed: aggregate limits enforced correctly")
}

// cleanupTestProcesses kills any Cloud Hypervisor processes started during test
func cleanupTestProcesses(t *testing.T, mgr *manager) {
	t.Helper()
	instances, err := mgr.ListInstances(context.Background())
	if err != nil {
		return
	}
	for _, inst := range instances {
		if inst.StoredMetadata.HypervisorPID != nil {
			pid := *inst.StoredMetadata.HypervisorPID
			if err := syscall.Kill(pid, 0); err == nil {
				t.Logf("Cleaning up hypervisor process: PID %d", pid)
				syscall.Kill(pid, syscall.SIGKILL)
			}
		}
	}
}
