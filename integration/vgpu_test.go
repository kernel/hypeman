package integration

import (
	"bytes"
	"context"
	"os"
	"testing"
	"time"

	"github.com/kernel/hypeman/cmd/api/config"
	"github.com/kernel/hypeman/lib/devices"
	"github.com/kernel/hypeman/lib/guest"
	"github.com/kernel/hypeman/lib/hypervisor"
	"github.com/kernel/hypeman/lib/images"
	"github.com/kernel/hypeman/lib/instances"
	"github.com/kernel/hypeman/lib/network"
	"github.com/kernel/hypeman/lib/paths"
	"github.com/kernel/hypeman/lib/system"
	"github.com/kernel/hypeman/lib/volumes"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestVGPU is an integration test that verifies vGPU (SR-IOV mdev) support works.
//
// This test automatically detects vGPU availability and skips if:
//   - No SR-IOV VFs are found in /sys/class/mdev_bus/
//   - No vGPU profiles are available
//   - Not running as root (required for mdev creation)
//   - KVM is not available
//
// To run manually:
//
//	sudo go test -v -run TestVGPU -timeout 5m ./integration/...
//
// Note: This test verifies mdev creation and PCI device visibility inside the VM.
// It does NOT test nvidia-smi or CUDA functionality since that requires NVIDIA
// guest drivers pre-installed in the image.
func TestVGPU(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	// Auto-detect vGPU availability - skip if prerequisites not met
	skipReason, profile := checkVGPUTestPrerequisites()
	if skipReason != "" {
		t.Skip(skipReason)
	}

	t.Logf("vGPU test prerequisites met, using profile: %s", profile)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	// Set up test environment
	tmpDir := t.TempDir()
	p := paths.New(tmpDir)

	cfg := &config.Config{
		DataDir:    tmpDir,
		BridgeName: "vmbr0",
		SubnetCIDR: "10.100.0.0/16",
		DNSServer:  "1.1.1.1",
	}

	// Create managers
	imageManager, err := images.NewManager(p, 1, nil)
	require.NoError(t, err)

	systemManager := system.NewManager(p)
	networkManager := network.NewManager(p, cfg, nil)
	deviceManager := devices.NewManager(p)
	volumeManager := volumes.NewManager(p, 0, nil)

	limits := instances.ResourceLimits{
		MaxOverlaySize:       100 * 1024 * 1024 * 1024,
		MaxVcpusPerInstance:  0,
		MaxMemoryPerInstance: 0,
		MaxTotalVcpus:        0,
		MaxTotalMemory:       0,
	}

	instanceManager := instances.NewManager(p, imageManager, systemManager, networkManager, deviceManager, volumeManager, limits, "", nil, nil)

	// Track instance ID for cleanup
	var instanceID string

	// Cleanup any orphaned instances and mdevs
	t.Cleanup(func() {
		if instanceID != "" {
			t.Log("Cleanup: Deleting instance...")
			instanceManager.DeleteInstance(ctx, instanceID)
		}
	})

	// Step 1: Ensure system files (kernel, initrd)
	t.Log("Step 1: Ensuring system files...")
	err = systemManager.EnsureSystemFiles(ctx)
	require.NoError(t, err)
	t.Log("System files ready")

	// Step 2: Pull alpine image (lightweight for testing)
	imageName := "docker.io/library/alpine:latest"
	t.Log("Step 2: Pulling alpine image...")
	_, err = imageManager.CreateImage(ctx, images.CreateImageRequest{
		Name: imageName,
	})
	require.NoError(t, err)

	// Wait for image to be ready
	t.Log("Waiting for image build...")
	var img *images.Image
	for i := 0; i < 120; i++ {
		img, err = imageManager.GetImage(ctx, imageName)
		if err == nil && img.Status == images.StatusReady {
			break
		}
		if img != nil && img.Status == images.StatusFailed {
			errMsg := "unknown"
			if img.Error != nil {
				errMsg = *img.Error
			}
			t.Fatalf("Image build failed: %s", errMsg)
		}
		time.Sleep(1 * time.Second)
	}
	require.NotNil(t, img, "Image should exist")
	require.Equal(t, images.StatusReady, img.Status, "Image should be ready")
	t.Log("Image ready")

	// Step 3: Check GPU resources BEFORE creating instance
	t.Log("Step 3: Recording GPU resources before instance creation...")
	profilesBefore, err := devices.ListGPUProfiles()
	require.NoError(t, err, "should list GPU profiles")
	var availableBefore int
	for _, p := range profilesBefore {
		if p.Name == profile {
			availableBefore = p.Available
			break
		}
	}
	require.Greater(t, availableBefore, 0, "profile should have availability before creation")
	t.Logf("Profile %q available instances before: %d", profile, availableBefore)

	// Step 4: Create instance with vGPU using QEMU hypervisor
	// QEMU is required for vGPU/mdev passthrough with NVIDIA's vGPU manager
	t.Log("Step 4: Creating instance with vGPU profile:", profile)
	inst, err := instanceManager.CreateInstance(ctx, instances.CreateInstanceRequest{
		Name:           "vgpu-test",
		Image:          imageName,
		Size:           2 * 1024 * 1024 * 1024, // 2GB
		HotplugSize:    512 * 1024 * 1024,
		OverlaySize:    1024 * 1024 * 1024,
		Vcpus:          2,
		NetworkEnabled: false, // No network needed for this test
		Hypervisor:     hypervisor.TypeQEMU,
		GPU: &instances.GPUConfig{
			Profile: profile,
		},
	})
	require.NoError(t, err)
	instanceID = inst.Id
	t.Logf("Instance created: %s", inst.Id)

	// Verify mdev UUID was assigned
	require.NotEmpty(t, inst.GPUMdevUUID, "Instance should have mdev UUID assigned")
	t.Logf("mdev UUID: %s", inst.GPUMdevUUID)

	// Step 5: Check GPU resources AFTER creating instance
	t.Run("ResourcesDecrementedAfterCreation", func(t *testing.T) {
		profilesAfter, err := devices.ListGPUProfiles()
		require.NoError(t, err, "should list GPU profiles after creation")

		var availableAfter int
		for _, p := range profilesAfter {
			if p.Name == profile {
				availableAfter = p.Available
				break
			}
		}

		t.Logf("Profile %q available instances after: %d (was %d)", profile, availableAfter, availableBefore)
		assert.Less(t, availableAfter, availableBefore, "available instances should decrease after creating VM")
	})

	// Step 6: Verify mdev was created in sysfs
	t.Run("MdevCreated", func(t *testing.T) {
		mdevPath := "/sys/bus/mdev/devices/" + inst.GPUMdevUUID
		_, err := os.Stat(mdevPath)
		assert.NoError(t, err, "mdev device should exist at %s", mdevPath)
		t.Logf("mdev exists at: %s", mdevPath)
	})

	// Step 7: Wait for guest agent to be ready
	t.Log("Step 7: Waiting for guest agent...")
	err = waitForGuestAgent(ctx, instanceManager, inst.Id, 60*time.Second)
	require.NoError(t, err, "guest agent should be ready")

	// Step 8: Verify GPU is visible inside VM via PCI
	t.Run("GPUVisibleInVM", func(t *testing.T) {
		actualInst, err := instanceManager.GetInstance(ctx, inst.Id)
		require.NoError(t, err)

		dialer, err := hypervisor.NewVsockDialer(actualInst.HypervisorType, actualInst.VsockSocket, actualInst.VsockCID)
		require.NoError(t, err)

		// Check for NVIDIA vendor ID (0x10de) in guest PCI devices
		var stdout, stderr bytes.Buffer
		checkGPUCmd := "cat /sys/bus/pci/devices/*/vendor 2>/dev/null | grep -i 10de && echo 'NVIDIA_FOUND' || echo 'NO_NVIDIA'"

		_, err = guest.ExecIntoInstance(ctx, dialer, guest.ExecOptions{
			Command: []string{"/bin/sh", "-c", checkGPUCmd},
			Stdout:  &stdout,
			Stderr:  &stderr,
			TTY:     false,
		})
		require.NoError(t, err, "exec should work")

		output := stdout.String()
		t.Logf("GPU check output: %s", output)

		assert.Contains(t, output, "NVIDIA_FOUND", "NVIDIA GPU (vendor 0x10de) should be visible in guest")
	})

	// Step 9: Check instance GPU info is correct
	t.Run("InstanceGPUInfo", func(t *testing.T) {
		actualInst, err := instanceManager.GetInstance(ctx, inst.Id)
		require.NoError(t, err)

		assert.Equal(t, profile, actualInst.GPUProfile, "GPU profile should match")
		assert.NotEmpty(t, actualInst.GPUMdevUUID, "mdev UUID should be set")
		t.Logf("Instance GPU: profile=%s, mdev=%s", actualInst.GPUProfile, actualInst.GPUMdevUUID)
	})

	t.Log("✅ vGPU test PASSED!")
}

// checkVGPUTestPrerequisites checks if vGPU test can run.
// Returns (skipReason, profileName) - skipReason is empty if all prerequisites are met.
func checkVGPUTestPrerequisites() (string, string) {
	// Check KVM
	if _, err := os.Stat("/dev/kvm"); os.IsNotExist(err) {
		return "vGPU test requires /dev/kvm", ""
	}

	// Check for root (required for mdev creation via sysfs)
	if os.Geteuid() != 0 {
		return "vGPU test requires root (sudo) for mdev creation", ""
	}

	// Check for vGPU mode (SR-IOV VFs present)
	mode := devices.DetectHostGPUMode()
	if mode != devices.GPUModeVGPU {
		return "vGPU test requires SR-IOV VFs in /sys/class/mdev_bus/", ""
	}

	// Check for available profiles
	profiles, err := devices.ListGPUProfiles()
	if err != nil {
		return "vGPU test failed to list profiles: " + err.Error(), ""
	}
	if len(profiles) == 0 {
		return "vGPU test requires at least one GPU profile", ""
	}

	// Find a profile with available instances
	for _, p := range profiles {
		if p.Available > 0 {
			return "", p.Name
		}
	}

	return "vGPU test requires at least one available VF (all VFs are in use)", ""
}

