package devices_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/onkernel/hypeman/cmd/api/config"
	"github.com/onkernel/hypeman/lib/devices"
	"github.com/onkernel/hypeman/lib/exec"
	"github.com/onkernel/hypeman/lib/images"
	"github.com/onkernel/hypeman/lib/instances"
	"github.com/onkernel/hypeman/lib/network"
	"github.com/onkernel/hypeman/lib/paths"
	"github.com/onkernel/hypeman/lib/system"
	"github.com/onkernel/hypeman/lib/volumes"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestGPUPassthrough is an E2E test that verifies GPU passthrough works.
//
// This test automatically detects GPU availability and skips if:
//   - No NVIDIA GPU is found
//   - IOMMU is not enabled
//   - VFIO modules are not loaded
//   - Not running as root
//
// To run manually:
//
//	sudo env PATH=$PATH:/sbin:/usr/sbin go test -v -run TestGPUPassthrough ./lib/devices/...
//
// WARNING: This test will unbind the GPU from the nvidia driver, which may
// disrupt other processes using the GPU. The test attempts to restore the
// nvidia driver binding on cleanup.
func TestGPUPassthrough(t *testing.T) {
	ctx := context.Background()

	// Auto-detect GPU availability - skip if prerequisites not met
	skipReason := checkGPUTestPrerequisites()
	if skipReason != "" {
		t.Skip(skipReason)
	}

	// Log that prerequisites passed
	groups, _ := os.ReadDir("/sys/kernel/iommu_groups")
	t.Logf("GPU test prerequisites met: %d IOMMU groups found", len(groups))

	// Setup test infrastructure
	tmpDir := t.TempDir()
	p := paths.New(tmpDir)

	cfg := &config.Config{
		DataDir:    tmpDir,
		BridgeName: "vmbr0",
		SubnetCIDR: "10.100.0.0/16",
		DNSServer:  "1.1.1.1",
	}

	// Initialize managers (nil meter/tracer disables metrics/tracing)
	imageMgr, err := images.NewManager(p, 1, nil)
	require.NoError(t, err)

	systemMgr := system.NewManager(p)
	networkMgr := network.NewManager(p, cfg, nil)
	deviceMgr := devices.NewManager(p)
	volumeMgr := volumes.NewManager(p, 100*1024*1024*1024, nil) // 100GB max volume storage
	limits := instances.ResourceLimits{
		MaxOverlaySize: 100 * 1024 * 1024 * 1024, // 100GB
	}
	instanceMgr := instances.NewManager(p, imageMgr, systemMgr, networkMgr, deviceMgr, volumeMgr, limits, nil, nil)

	// Step 1: Discover available GPUs
	t.Log("Step 1: Discovering available GPUs...")
	availableDevices, err := deviceMgr.ListAvailableDevices(ctx)
	require.NoError(t, err)

	// Find an NVIDIA GPU
	var targetGPU *devices.AvailableDevice
	for _, d := range availableDevices {
		if strings.Contains(strings.ToLower(d.VendorName), "nvidia") {
			targetGPU = &d
			break
		}
	}
	require.NotNil(t, targetGPU, "No NVIDIA GPU found on this system")
	driverStr := "none"
	if targetGPU.CurrentDriver != nil {
		driverStr = *targetGPU.CurrentDriver
	}
	t.Logf("Found NVIDIA GPU: %s at %s (driver: %s)", targetGPU.DeviceName, targetGPU.PCIAddress, driverStr)

	// Check GPU is in a usable state (has a driver bound)
	if targetGPU.CurrentDriver == nil || *targetGPU.CurrentDriver == "" {
		t.Skip("GPU has no driver bound - may need reboot to recover. Run: sudo reboot")
	}

	// Verify the driver path exists (GPU not in broken state)
	driverPath := filepath.Join("/sys/bus/pci/devices", targetGPU.PCIAddress, "driver")
	if _, err := os.Stat(driverPath); os.IsNotExist(err) {
		t.Skipf("GPU driver symlink missing at %s - GPU in broken state, reboot required", driverPath)
	}

	// Step 2: Register the GPU
	t.Log("Step 2: Registering GPU...")
	device, err := deviceMgr.CreateDevice(ctx, devices.CreateDeviceRequest{
		Name:       "test-gpu",
		PCIAddress: targetGPU.PCIAddress,
	})
	require.NoError(t, err)
	t.Logf("Registered device: %s (ID: %s)", device.Name, device.Id)

	// Store original driver for cleanup
	originalDriver := driverStr

	// Cleanup: always unregister device and try to restore original driver
	t.Cleanup(func() {
		t.Log("Cleanup: Deleting registered device...")
		deviceMgr.DeleteDevice(ctx, device.Id)

		// Try to restore original driver binding via driver_probe
		if originalDriver != "" && originalDriver != "none" && originalDriver != "vfio-pci" {
			t.Logf("Cleanup: Triggering driver probe to restore %s driver...", originalDriver)
			// Use driver_probe to let the kernel find and bind the right driver
			probePath := "/sys/bus/pci/drivers_probe"
			if err := os.WriteFile(probePath, []byte(targetGPU.PCIAddress), 0200); err != nil {
				t.Logf("Warning: Could not trigger driver probe: %v (may need reboot)", err)
			} else {
				t.Logf("Cleanup: Driver probe triggered for %s", targetGPU.PCIAddress)
			}
		}
	})

	// Step 3: Ensure system files (kernel, initrd)
	t.Log("Step 3: Ensuring system files...")
	err = systemMgr.EnsureSystemFiles(ctx)
	require.NoError(t, err)
	t.Log("System files ready")

	// Step 4: Pull nginx:alpine (nginx keeps running unlike plain alpine which exits immediately)
	t.Log("Step 4: Pulling nginx:alpine image...")
	createdImg, createErr := imageMgr.CreateImage(ctx, images.CreateImageRequest{
		Name: "docker.io/library/nginx:alpine",
	})
	require.NoError(t, createErr, "CreateImage should succeed")
	t.Logf("CreateImage returned: name=%s, status=%s", createdImg.Name, createdImg.Status)

	// Use the name returned from CreateImage (it may be normalized)
	imageName := createdImg.Name

	// Wait for image to be ready
	var img *images.Image
	for i := 0; i < 90; i++ {
		img, err = imageMgr.GetImage(ctx, imageName)
		if err != nil {
			if i < 5 || i%10 == 0 {
				t.Logf("GetImage attempt %d: error=%v", i+1, err)
			}
		} else {
			if i < 5 || i%10 == 0 {
				t.Logf("GetImage attempt %d: status=%s", i+1, img.Status)
			}
			if img.Status == images.StatusReady {
				break
			}
			if img.Status == images.StatusFailed {
				errMsg := "unknown"
				if img.Error != nil {
					errMsg = *img.Error
				}
				t.Fatalf("Image build failed: %s", errMsg)
			}
		}
		time.Sleep(1 * time.Second)
	}
	require.NotNil(t, img, "Image should exist after 90 seconds")
	require.Equal(t, images.StatusReady, img.Status, "Image should be ready")
	t.Log("Image ready")

	// Step 5: Create instance with GPU (with timeout to prevent hang on VFIO issues)
	t.Log("Step 5: Creating instance with GPU...")
	createCtx, createCancel := context.WithTimeout(ctx, 60*time.Second)
	defer createCancel()

	inst, err := instanceMgr.CreateInstance(createCtx, instances.CreateInstanceRequest{
		Name:           "gpu-test",
		Image:          "docker.io/library/nginx:alpine",
		Size:           512 * 1024 * 1024,
		HotplugSize:    512 * 1024 * 1024,
		OverlaySize:    10 * 1024 * 1024 * 1024,
		Vcpus:          1,
		NetworkEnabled: false,
		Devices:        []string{"test-gpu"},
		Env:            map[string]string{},
	})
	require.NoError(t, err)
	t.Logf("Instance created: %s", inst.Id)

	// Cleanup: always delete instance
	t.Cleanup(func() {
		t.Log("Cleanup: Deleting instance...")
		instanceMgr.DeleteInstance(ctx, inst.Id)
	})

	// Step 6: Wait for instance to be ready
	t.Log("Step 6: Waiting for instance to be ready...")
	err = waitForInstanceReady(ctx, t, instanceMgr, inst.Id, 30*time.Second)
	require.NoError(t, err)
	t.Log("Instance is ready")

	// Step 7: Verify GPU is visible inside VM
	// Note: Alpine doesn't have lspci, so we check /sys/bus/pci directly for NVIDIA vendor ID (0x10de)
	t.Log("Step 7: Verifying GPU visibility inside VM...")
	actualInst, err := instanceMgr.GetInstance(ctx, inst.Id)
	require.NoError(t, err)

	// Create a context with timeout for exec operations
	execCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	// Retry exec a few times (exec agent may need time to start)
	var stdout, stderr outputBuffer
	var execErr error
	// Command to find NVIDIA devices by checking vendor IDs (0x10de = NVIDIA)
	checkGPUCmd := "cat /sys/bus/pci/devices/*/vendor 2>/dev/null | grep -i 10de && echo 'NVIDIA_FOUND'"

	for i := 0; i < 15; i++ {
		stdout = outputBuffer{}
		stderr = outputBuffer{}

		_, execErr = exec.ExecIntoInstance(execCtx, actualInst.VsockSocket, exec.ExecOptions{
			Command: []string{"/bin/sh", "-c", checkGPUCmd},
			Stdin:   nil,
			Stdout:  &stdout,
			Stderr:  &stderr,
			TTY:     false,
		})

		if execErr == nil {
			break
		}
		t.Logf("Exec attempt %d/15 failed: %v", i+1, execErr)
		time.Sleep(1 * time.Second)
	}
	if execErr != nil {
		// Print console log for debugging
		p := paths.New(tmpDir)
		consoleLogPath := p.InstanceConsoleLog(inst.Id)
		if consoleLog, err := os.ReadFile(consoleLogPath); err == nil {
			t.Logf("=== VM Console Log ===\n%s\n=== End Console Log ===", string(consoleLog))
		} else {
			t.Logf("Could not read console log: %v", err)
		}
	}
	require.NoError(t, execErr, "exec should succeed")

	pciOutput := stdout.String()
	t.Logf("PCI vendor check output:\n%s", pciOutput)

	// Verify NVIDIA device is visible (vendor ID 0x10de)
	assert.True(t,
		strings.Contains(pciOutput, "NVIDIA_FOUND") ||
			strings.Contains(strings.ToLower(pciOutput), "10de"),
		"NVIDIA GPU (vendor 0x10de) should be visible in guest")

	t.Log("✅ GPU passthrough test PASSED!")
}

// checkGPUTestPrerequisites checks if GPU passthrough test can run.
// Returns empty string if all prerequisites are met, otherwise returns skip reason.
func checkGPUTestPrerequisites() string {
	// Check KVM
	if _, err := os.Stat("/dev/kvm"); os.IsNotExist(err) {
		return "GPU passthrough test requires /dev/kvm"
	}

	// Check VFIO modules
	if _, err := os.Stat("/dev/vfio/vfio"); os.IsNotExist(err) {
		return "GPU passthrough test requires VFIO (modprobe vfio_pci vfio_iommu_type1)"
	}

	// Check IOMMU is enabled by looking for IOMMU groups
	groups, err := os.ReadDir("/sys/kernel/iommu_groups")
	if err != nil || len(groups) == 0 {
		return "GPU passthrough test requires IOMMU (intel_iommu=on or amd_iommu=on)"
	}

	// Check for NVIDIA GPU
	available, err := devices.DiscoverAvailableDevices()
	if err != nil {
		return "GPU passthrough test failed to discover devices: " + err.Error()
	}

	hasNvidiaGPU := false
	for _, d := range available {
		if strings.Contains(strings.ToLower(d.VendorName), "nvidia") {
			hasNvidiaGPU = true
			break
		}
	}
	if !hasNvidiaGPU {
		return "GPU passthrough test requires an NVIDIA GPU"
	}

	// GPU passthrough requires root (euid=0) for sysfs driver bind/unbind operations.
	// Unlike network operations which can use CAP_NET_ADMIN, sysfs file writes are
	// protected by standard Unix DAC (file permissions), not just capabilities.
	// The files in /sys/bus/pci/drivers/ are owned by root with mode 0200.
	if os.Geteuid() != 0 {
		return "GPU passthrough test requires root (sudo) for sysfs driver operations"
	}

	return "" // All prerequisites met
}

func waitForInstanceReady(ctx context.Context, t *testing.T, mgr instances.Manager, id string, timeout time.Duration) error {
	t.Helper()

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		inst, err := mgr.GetInstance(ctx, id)
		if err != nil {
			time.Sleep(500 * time.Millisecond)
			continue
		}

		if inst.State == instances.StateRunning {
			// Additional check: wait a bit for exec agent
			time.Sleep(2 * time.Second)
			return nil
		}

		time.Sleep(500 * time.Millisecond)
	}

	return context.DeadlineExceeded
}

type outputBuffer struct {
	buf bytes.Buffer
}

func (b *outputBuffer) Write(p []byte) (n int, err error) {
	return b.buf.Write(p)
}

func (b *outputBuffer) String() string {
	return b.buf.String()
}
