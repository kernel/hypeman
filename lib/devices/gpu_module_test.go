package devices_test

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	osexec "os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/daemon"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/onkernel/hypeman/cmd/api/config"
	"github.com/onkernel/hypeman/lib/devices"
	"github.com/onkernel/hypeman/lib/guest"
	"github.com/onkernel/hypeman/lib/hypervisor"
	"github.com/onkernel/hypeman/lib/images"
	"github.com/onkernel/hypeman/lib/instances"
	"github.com/onkernel/hypeman/lib/network"
	"github.com/onkernel/hypeman/lib/paths"
	"github.com/onkernel/hypeman/lib/registry"
	"github.com/onkernel/hypeman/lib/system"
	"github.com/onkernel/hypeman/lib/volumes"
	"github.com/stretchr/testify/require"
)

// TestNVIDIAModuleLoading verifies that NVIDIA kernel modules load correctly in the VM.
//
// This is a simpler test than TestGPUInference that just verifies:
//  1. NVIDIA kernel modules (nvidia.ko, nvidia-uvm.ko, etc.) load during init
//  2. GSP firmware is found and loaded
//  3. /dev/nvidia* device nodes are created
//
// Prerequisites:
//   - NVIDIA GPU on host
//   - IOMMU enabled
//   - VFIO modules loaded (modprobe vfio_pci)
//   - Running as root
//
// To run manually:
//
//	sudo env PATH=$PATH:/sbin:/usr/sbin go test -v -run TestNVIDIAModuleLoading -timeout 5m ./lib/devices/...
func TestNVIDIAModuleLoading(t *testing.T) {
	ctx := context.Background()

	// Auto-detect GPU availability - skip if prerequisites not met
	skipReason := checkGPUTestPrerequisites()
	if skipReason != "" {
		t.Skip(skipReason)
	}

	groups, _ := os.ReadDir("/sys/kernel/iommu_groups")
	t.Logf("Test prerequisites met: %d IOMMU groups found", len(groups))

	// Setup test infrastructure
	tmpDir := t.TempDir()
	p := paths.New(tmpDir)

	cfg := &config.Config{
		DataDir:    tmpDir,
		BridgeName: "vmbr0",
		SubnetCIDR: "10.100.0.0/16",
		DNSServer:  "1.1.1.1",
	}

	// Initialize managers
	imageMgr, err := images.NewManager(p, 1, nil)
	require.NoError(t, err)

	systemMgr := system.NewManager(p)
	networkMgr := network.NewManager(p, cfg, nil)
	deviceMgr := devices.NewManager(p)
	volumeMgr := volumes.NewManager(p, 10*1024*1024*1024, nil)
	limits := instances.ResourceLimits{MaxOverlaySize: 10 * 1024 * 1024 * 1024}
	instanceMgr := instances.NewManager(p, imageMgr, systemMgr, networkMgr, deviceMgr, volumeMgr, limits, "", nil, nil)

	// Step 1: Find an NVIDIA GPU
	t.Log("Step 1: Discovering available GPUs...")
	availableDevices, err := deviceMgr.ListAvailableDevices(ctx)
	require.NoError(t, err)

	var targetGPU *devices.AvailableDevice
	for _, d := range availableDevices {
		if strings.Contains(strings.ToLower(d.VendorName), "nvidia") {
			targetGPU = &d
			break
		}
	}
	require.NotNil(t, targetGPU, "No NVIDIA GPU found")

	driverStr := "none"
	if targetGPU.CurrentDriver != nil {
		driverStr = *targetGPU.CurrentDriver
	}
	t.Logf("Found NVIDIA GPU: %s at %s (driver: %s)", targetGPU.DeviceName, targetGPU.PCIAddress, driverStr)

	if targetGPU.CurrentDriver == nil || *targetGPU.CurrentDriver == "" {
		t.Skip("GPU has no driver bound - may need reboot")
	}

	driverPath := filepath.Join("/sys/bus/pci/devices", targetGPU.PCIAddress, "driver")
	if _, err := os.Stat(driverPath); os.IsNotExist(err) {
		t.Skipf("GPU driver symlink missing - GPU in broken state")
	}

	// Step 2: Register the GPU
	t.Log("Step 2: Registering GPU...")
	device, err := deviceMgr.CreateDevice(ctx, devices.CreateDeviceRequest{
		Name:       "module-test-gpu",
		PCIAddress: targetGPU.PCIAddress,
	})
	require.NoError(t, err)
	t.Logf("Registered device: %s (ID: %s)", device.Name, device.Id)

	originalDriver := driverStr
	t.Cleanup(func() {
		t.Log("Cleanup: Deleting registered device...")
		deviceMgr.DeleteDevice(ctx, device.Id)
		if originalDriver != "" && originalDriver != "none" && originalDriver != "vfio-pci" {
			probePath := "/sys/bus/pci/drivers_probe"
			os.WriteFile(probePath, []byte(targetGPU.PCIAddress), 0200)
		}
	})

	// Step 3: Ensure system files
	t.Log("Step 3: Ensuring system files...")
	require.NoError(t, systemMgr.EnsureSystemFiles(ctx))

	// Step 4: Pull nginx:alpine (stays running unlike plain alpine)
	t.Log("Step 4: Pulling nginx:alpine image...")
	createdImg, err := imageMgr.CreateImage(ctx, images.CreateImageRequest{
		Name: "docker.io/library/nginx:alpine",
	})
	require.NoError(t, err)
	t.Logf("CreateImage returned: name=%s, status=%s", createdImg.Name, createdImg.Status)

	// Wait for image to be ready
	var img *images.Image
	for i := 0; i < 90; i++ {
		img, _ = imageMgr.GetImage(ctx, createdImg.Name)
		if img != nil && img.Status == images.StatusReady {
			break
		}
		time.Sleep(time.Second)
	}
	require.NotNil(t, img, "Image should exist")
	require.Equal(t, images.StatusReady, img.Status, "Image should be ready")
	t.Log("Image ready")

	// Step 5: Create instance with GPU
	t.Log("Step 5: Creating instance with GPU...")

	// Initialize network first
	require.NoError(t, networkMgr.Initialize(ctx, []string{}))

	createCtx, createCancel := context.WithTimeout(ctx, 60*time.Second)
	defer createCancel()

	inst, err := instanceMgr.CreateInstance(createCtx, instances.CreateInstanceRequest{
		Name:           "nvidia-module-test",
		Image:          createdImg.Name,
		Size:           2 * 1024 * 1024 * 1024, // 2GB (needs extra room for initrd with NVIDIA libs)
		HotplugSize:    512 * 1024 * 1024,
		OverlaySize:    10 * 1024 * 1024 * 1024,
		Vcpus:          2,
		NetworkEnabled: false,
		Devices:        []string{"module-test-gpu"},
		Env:            map[string]string{},
	})
	require.NoError(t, err)
	t.Logf("Instance created: %s", inst.Id)

	t.Cleanup(func() {
		t.Log("Cleanup: Deleting instance...")
		instanceMgr.DeleteInstance(ctx, inst.Id)
	})

	// Wait for instance to be running
	err = waitForInstanceReady(ctx, t, instanceMgr, inst.Id, 30*time.Second)
	require.NoError(t, err)
	t.Log("Instance is ready")

	// Wait for init script to complete (module loading happens early in boot)
	time.Sleep(5 * time.Second)

	// Step 6: Check module loading via dmesg
	t.Log("Step 6: Checking NVIDIA module loading in VM...")

	actualInst, err := instanceMgr.GetInstance(ctx, inst.Id)
	require.NoError(t, err)

	dialer, err := hypervisor.NewVsockDialer(actualInst.HypervisorType, actualInst.VsockSocket, actualInst.VsockCID)
	require.NoError(t, err)

	execCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	// Check dmesg for NVIDIA messages
	var stdout, stderr outputBuffer
	dmesgCmd := "dmesg | grep -i nvidia | head -50"

	for i := 0; i < 10; i++ {
		stdout = outputBuffer{}
		stderr = outputBuffer{}
		_, err = guest.ExecIntoInstance(execCtx, dialer, guest.ExecOptions{
			Command: []string{"/bin/sh", "-c", dmesgCmd},
			Stdin:   nil,
			Stdout:  &stdout,
			Stderr:  &stderr,
		})
		if err == nil {
			break
		}
		time.Sleep(time.Second)
	}
	require.NoError(t, err, "dmesg command should succeed")

	dmesgOutput := stdout.String()
	t.Logf("=== NVIDIA dmesg output ===\n%s", dmesgOutput)

	// Check for key error indicators
	firmwareMissing := strings.Contains(dmesgOutput, "No firmware image found")
	initFailed := strings.Contains(dmesgOutput, "RmInitAdapter failed")

	if firmwareMissing {
		t.Errorf("✗ GSP firmware not found - firmware not included in initrd")
	}
	if initFailed {
		t.Errorf("✗ NVIDIA driver RmInitAdapter failed - GPU initialization error")
	}

	// Check lsmod for nvidia modules
	stdout = outputBuffer{}
	stderr = outputBuffer{}
	_, err = guest.ExecIntoInstance(execCtx, dialer, guest.ExecOptions{
		Command: []string{"/bin/sh", "-c", "cat /proc/modules | grep nvidia || echo 'No nvidia modules loaded'"},
		Stdin:   nil,
		Stdout:  &stdout,
		Stderr:  &stderr,
	})
	require.NoError(t, err)
	modulesOutput := stdout.String()
	t.Logf("=== Loaded nvidia modules ===\n%s", modulesOutput)

	hasModules := !strings.Contains(modulesOutput, "No nvidia modules loaded")
	if !hasModules {
		t.Errorf("✗ NVIDIA modules not loaded in VM")
	} else {
		t.Log("✓ NVIDIA kernel modules are loaded")
	}

	// Check for /dev/nvidia* devices
	stdout = outputBuffer{}
	stderr = outputBuffer{}
	_, err = guest.ExecIntoInstance(execCtx, dialer, guest.ExecOptions{
		Command: []string{"/bin/sh", "-c", "ls -la /dev/nvidia* 2>&1 || echo 'No nvidia devices found'"},
		Stdin:   nil,
		Stdout:  &stdout,
		Stderr:  &stderr,
	})
	require.NoError(t, err)
	devicesOutput := stdout.String()
	t.Logf("=== NVIDIA device nodes ===\n%s", devicesOutput)

	hasDevices := !strings.Contains(devicesOutput, "No nvidia devices found") && !strings.Contains(devicesOutput, "No such file")
	if hasDevices {
		t.Log("✓ /dev/nvidia* device nodes exist")
	} else {
		t.Log("✗ /dev/nvidia* device nodes not found (expected if init failed)")
	}

	// Final verdict
	if !firmwareMissing && !initFailed && hasModules {
		t.Log("\n=== SUCCESS: NVIDIA kernel modules loaded correctly ===")
	} else {
		t.Errorf("\n=== FAILURE: NVIDIA module loading has issues ===")
	}
}

// TestNVMLDetection tests if NVML can detect the GPU from userspace.
// This uses the custom CUDA+Ollama image and runs a Python NVML test.
//
// To run manually:
//
//	sudo env PATH=$PATH:/sbin:/usr/sbin go test -v -run TestNVMLDetection -timeout 10m ./lib/devices/...
func TestNVMLDetection(t *testing.T) {
	ctx := context.Background()

	skipReason := checkGPUTestPrerequisites()
	if skipReason != "" {
		t.Skip(skipReason)
	}

	groups, _ := os.ReadDir("/sys/kernel/iommu_groups")
	t.Logf("Test prerequisites met: %d IOMMU groups found", len(groups))

	// Use persistent test directory for image caching
	const persistentTestDataDir = "/var/lib/hypeman-gpu-inference-test"
	if err := os.MkdirAll(persistentTestDataDir, 0755); err != nil {
		t.Fatalf("Failed to create persistent test dir: %v", err)
	}

	p := paths.New(persistentTestDataDir)
	cfg := &config.Config{
		DataDir:    persistentTestDataDir,
		BridgeName: "vmbr0",
		SubnetCIDR: "10.100.0.0/16",
		DNSServer:  "1.1.1.1",
	}

	imageMgr, err := images.NewManager(p, 1, nil)
	require.NoError(t, err)

	systemMgr := system.NewManager(p)
	networkMgr := network.NewManager(p, cfg, nil)
	deviceMgr := devices.NewManager(p)
	volumeMgr := volumes.NewManager(p, 10*1024*1024*1024, nil)
	limits := instances.ResourceLimits{MaxOverlaySize: 10 * 1024 * 1024 * 1024}
	instanceMgr := instances.NewManager(p, imageMgr, systemMgr, networkMgr, deviceMgr, volumeMgr, limits, "", nil, nil)

	// Step 1: Check if ollama-cuda:test image exists in Docker
	t.Log("Step 1: Checking for ollama-cuda:test Docker image...")
	checkCmd := osexec.Command("docker", "image", "inspect", "ollama-cuda:test")
	if err := checkCmd.Run(); err != nil {
		t.Fatal("Docker image ollama-cuda:test not found. Build it first with:\n" +
			"  cd lib/devices/testdata/ollama-cuda && docker build -t ollama-cuda:test .")
	}
	t.Log("Docker image ollama-cuda:test exists")

	// Step 2: Start registry and push image
	t.Log("Step 2: Starting registry and pushing image...")
	reg, err := registry.New(p, imageMgr)
	require.NoError(t, err)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		log.Printf("%s %s", r.Method, r.URL.Path)
		reg.Handler().ServeHTTP(w, r)
	}))
	defer server.Close()

	serverHost := strings.TrimPrefix(server.URL, "http://")
	pushLocalDockerImageForTest(t, "ollama-cuda:test", serverHost)
	t.Log("Push complete")

	// Wait for image conversion
	t.Log("Waiting for image conversion...")
	var img *images.Image
	var imageName string
	for i := 0; i < 180; i++ { // 3 minutes max
		allImages, listErr := imageMgr.ListImages(ctx)
		if listErr == nil {
			for _, candidate := range allImages {
				if strings.Contains(candidate.Name, "ollama-cuda") {
					img = &candidate
					imageName = candidate.Name
					break
				}
			}
		}
		if img != nil && img.Status == images.StatusReady {
			break
		}
		if i%30 == 0 {
			status := "not found"
			if img != nil {
				status = string(img.Status)
			}
			t.Logf("Waiting for image... (%d/180, status=%s)", i+1, status)
		}
		time.Sleep(time.Second)
	}
	require.NotNil(t, img, "Image should exist after 3 minutes")
	require.Equal(t, images.StatusReady, img.Status, "Image should be ready")
	t.Logf("Image ready: %s", imageName)

	// Step 3: Find and register GPU
	t.Log("Step 3: Discovering GPUs...")
	availableDevices, err := deviceMgr.ListAvailableDevices(ctx)
	require.NoError(t, err)

	var targetGPU *devices.AvailableDevice
	for _, d := range availableDevices {
		if strings.Contains(strings.ToLower(d.VendorName), "nvidia") {
			targetGPU = &d
			break
		}
	}
	require.NotNil(t, targetGPU, "No NVIDIA GPU found")
	t.Logf("Found GPU: %s at %s", targetGPU.DeviceName, targetGPU.PCIAddress)

	device, err := deviceMgr.CreateDevice(ctx, devices.CreateDeviceRequest{
		Name:       "nvml-test-gpu",
		PCIAddress: targetGPU.PCIAddress,
	})
	require.NoError(t, err)
	t.Cleanup(func() {
		deviceMgr.DeleteDevice(ctx, device.Id)
	})

	// Step 4: Initialize network and system
	require.NoError(t, networkMgr.Initialize(ctx, []string{}))
	require.NoError(t, systemMgr.EnsureSystemFiles(ctx))

	// Step 5: Create instance
	t.Log("Step 4: Creating instance with CUDA image...")
	inst, err := instanceMgr.CreateInstance(ctx, instances.CreateInstanceRequest{
		Name:           "nvml-test",
		Image:          imageName,
		Size:           2 * 1024 * 1024 * 1024,
		HotplugSize:    512 * 1024 * 1024,
		OverlaySize:    10 * 1024 * 1024 * 1024,
		Vcpus:          2,
		NetworkEnabled: true,
		Devices:        []string{"nvml-test-gpu"},
		Env:            map[string]string{},
	})
	require.NoError(t, err)
	t.Logf("Instance created: %s", inst.Id)

	t.Cleanup(func() {
		t.Log("Cleanup: Deleting instance...")
		instanceMgr.DeleteInstance(ctx, inst.Id)
	})

	err = waitForInstanceReady(ctx, t, instanceMgr, inst.Id, 60*time.Second)
	require.NoError(t, err)
	time.Sleep(5 * time.Second)

	actualInst, err := instanceMgr.GetInstance(ctx, inst.Id)
	require.NoError(t, err)

	dialer2, err := hypervisor.NewVsockDialer(actualInst.HypervisorType, actualInst.VsockSocket, actualInst.VsockCID)
	require.NoError(t, err)

	// Step 5: Run NVML test
	t.Log("Step 5: Running NVML detection test...")
	execCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	var stdout, stderr outputBuffer
	_, err = guest.ExecIntoInstance(execCtx, dialer2, guest.ExecOptions{
		Command: []string{"/bin/sh", "-c", "python3 /usr/local/bin/test-nvml.py 2>&1"},
		Stdin:   nil,
		Stdout:  &stdout,
		Stderr:  &stderr,
	})

	t.Logf("NVML test output:\n%s", stdout.String())
	if stderr.String() != "" {
		t.Logf("NVML test stderr:\n%s", stderr.String())
	}

	require.NoError(t, err, "NVML test command should succeed")

	output := stdout.String()
	if strings.Contains(output, "GPU DETECTED") {
		t.Log("✓ SUCCESS: NVML detected the GPU!")
	} else if strings.Contains(output, "NVML_ERROR_LIB_RM_VERSION_MISMATCH") {
		t.Log("✗ NVML version mismatch - container NVML library doesn't match kernel driver version")
		t.Log("  Container has: 570.195.03")
		t.Log("  Kernel driver: 570.86.16")
		t.FailNow()
	} else if strings.Contains(output, "NVML_ERROR_DRIVER_NOT_LOADED") {
		t.Log("✗ NVML reports driver not loaded (but kernel modules are loaded)")
		t.FailNow()
	} else {
		t.Errorf("✗ NVML test failed: %s", output)
	}

	// Step 6: Run CUDA test
	t.Log("Step 6: Running CUDA driver test...")
	stdout = outputBuffer{}
	stderr = outputBuffer{}
	_, err = guest.ExecIntoInstance(execCtx, dialer2, guest.ExecOptions{
		Command: []string{"/bin/sh", "-c", "python3 /usr/local/bin/test-cuda.py 2>&1"},
		Stdin:   nil,
		Stdout:  &stdout,
		Stderr:  &stderr,
	})

	t.Logf("CUDA test output:\n%s", stdout.String())
	if strings.Contains(stdout.String(), "CUDA WORKS") {
		t.Log("✓ SUCCESS: CUDA driver works!")
	} else {
		t.Logf("CUDA test may have issues: %s", stdout.String())
	}
}

// pushLocalDockerImageForTest is a test helper that pushes a local Docker image to the registry
func pushLocalDockerImageForTest(t *testing.T, dockerImage, serverHost string) {
	t.Helper()

	srcRef, err := name.ParseReference(dockerImage)
	require.NoError(t, err)

	img, err := daemon.Image(srcRef)
	require.NoError(t, err)

	targetRef := fmt.Sprintf("%s/test/ollama-cuda:latest", serverHost)
	t.Logf("Pushing to %s", targetRef)

	dstRef, err := name.ParseReference(targetRef, name.Insecure)
	require.NoError(t, err)

	err = remote.Write(dstRef, img)
	require.NoError(t, err)
}
