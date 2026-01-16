package devices_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	osExec "os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/daemon"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/kernel/hypeman/cmd/api/config"
	"github.com/kernel/hypeman/lib/devices"
	"github.com/kernel/hypeman/lib/guest"
	"github.com/kernel/hypeman/lib/hypervisor"
	"github.com/kernel/hypeman/lib/images"
	"github.com/kernel/hypeman/lib/instances"
	"github.com/kernel/hypeman/lib/network"
	"github.com/kernel/hypeman/lib/paths"
	"github.com/kernel/hypeman/lib/registry"
	"github.com/kernel/hypeman/lib/system"
	"github.com/kernel/hypeman/lib/volumes"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// persistentTestDataDir is used to persist volumes between test runs.
// This allows the ollama model cache to survive across test executions.
// Note: Uses /var/lib instead of /tmp because /tmp often has limited space
// and the custom CUDA+Ollama image is ~4GB.
const persistentTestDataDir = "/var/lib/hypeman-gpu-inference-test"

// ollamaCudaDockerImage is the name we use for the custom CUDA+Ollama image
const ollamaCudaDockerImage = "ollama-cuda:test"

// TestGPUInference is an E2E test that verifies Ollama GPU inference works with VFIO passthrough.
//
// This test:
//  1. Builds a custom Docker image with NVIDIA CUDA runtime + Ollama (no drivers)
//  2. Pushes the image to hypeman's test registry
//  3. Launches a VM with GPU passthrough + the image
//  4. Installs NVIDIA driver at runtime via DKMS (builds kernel modules)
//  5. Runs `ollama run tinyllama` to perform GPU-accelerated inference
//  6. Verifies the model generates output
//
// The custom image bundles CUDA libraries. NVIDIA kernel drivers are installed
// at runtime via DKMS, which works because hypeman automatically installs
// kernel headers during VM boot.
//
// Prerequisites:
//   - NVIDIA GPU on host
//   - IOMMU enabled
//   - VFIO modules loaded (modprobe vfio_pci)
//   - Docker installed (for building custom image)
//   - Running as root
//
// To run manually:
//
//	sudo env PATH=$PATH:/sbin:/usr/sbin go test -v -run TestGPUInference -timeout 30m ./lib/devices/...
//
// To clean up:
//
//	sudo rm -rf /var/lib/hypeman-gpu-inference-test
//	docker rmi ollama-cuda:test
func TestGPUInference(t *testing.T) {
	ctx := context.Background()

	// Auto-detect GPU availability - skip if prerequisites not met
	skipReason := checkGPUTestPrerequisites()
	if skipReason != "" {
		t.Skip(skipReason)
	}

	// Check Docker is available
	if _, err := osExec.LookPath("docker"); err != nil {
		t.Skip("Docker not installed - required for building custom CUDA image")
	}

	groups, _ := os.ReadDir("/sys/kernel/iommu_groups")
	t.Logf("GPU inference test prerequisites met: %d IOMMU groups found", len(groups))

	// Use persistent directory for volume storage (survives between test runs)
	if err := os.MkdirAll(persistentTestDataDir, 0755); err != nil {
		t.Fatalf("Failed to create persistent test directory: %v", err)
	}
	p := paths.New(persistentTestDataDir)

	cfg := &config.Config{
		DataDir:    persistentTestDataDir,
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
	volumeMgr := volumes.NewManager(p, 100*1024*1024*1024, nil)
	limits := instances.ResourceLimits{
		MaxOverlaySize: 100 * 1024 * 1024 * 1024,
	}
	instanceMgr := instances.NewManager(p, imageMgr, systemMgr, networkMgr, deviceMgr, volumeMgr, limits, "", nil, nil)

	// Step 1: Build custom CUDA+Ollama image
	t.Log("Step 1: Building custom CUDA+Ollama Docker image...")
	dockerfilePath := getDockerfilePath(t)
	buildCustomCudaImage(t, dockerfilePath, ollamaCudaDockerImage)

	// Step 2: Set up test registry and push the image
	t.Log("Step 2: Pushing custom image to hypeman registry...")
	reg, err := registry.New(p, imageMgr)
	require.NoError(t, err)

	router := chi.NewRouter()
	router.Mount("/v2", reg.Handler())
	ts := httptest.NewServer(router)
	t.Cleanup(ts.Close)

	serverHost := strings.TrimPrefix(ts.URL, "http://")
	pushLocalDockerImage(t, ollamaCudaDockerImage, serverHost)
	t.Log("Push complete")

	// Wait for image conversion - find image by listing since digest may change during Docker->OCI conversion
	t.Log("Waiting for image conversion...")
	var img *images.Image
	var imageName string
	for i := 0; i < 300; i++ { // 5 minutes for large CUDA image
		// List images and find our ollama-cuda image
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
		if img != nil && img.Status == images.StatusFailed {
			errMsg := "unknown"
			if img.Error != nil {
				errMsg = *img.Error
			}
			t.Fatalf("Image conversion failed: %s", errMsg)
		}
		if i%30 == 0 {
			status := "not found"
			if img != nil {
				status = string(img.Status)
			}
			t.Logf("Waiting for image conversion... (%d/300, status=%s)", i+1, status)
		}
		time.Sleep(time.Second)
	}
	require.NotNil(t, img, "Image should exist after 5 minutes")
	require.Equal(t, images.StatusReady, img.Status, "Image should be ready")
	t.Logf("Image ready: %s (digest: %s)", imageName, img.Digest)

	// Step 3: Discover and register GPU
	t.Log("Step 3: Discovering available GPUs...")
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

	// Register GPU
	t.Log("Step 4: Registering GPU...")
	device, err := deviceMgr.GetDevice(ctx, "inference-gpu")
	if err != nil {
		device, err = deviceMgr.CreateDevice(ctx, devices.CreateDeviceRequest{
			Name:       "inference-gpu",
			PCIAddress: targetGPU.PCIAddress,
		})
		require.NoError(t, err)
		t.Logf("Registered new device: %s (ID: %s)", device.Name, device.Id)
	} else {
		t.Logf("Using existing device: %s (ID: %s)", device.Name, device.Id)
	}

	originalDriver := driverStr
	t.Cleanup(func() {
		t.Log("Cleanup: Deleting registered device...")
		deviceMgr.DeleteDevice(ctx, device.Id)
		if originalDriver != "" && originalDriver != "none" && originalDriver != "vfio-pci" {
			probePath := "/sys/bus/pci/drivers_probe"
			os.WriteFile(probePath, []byte(targetGPU.PCIAddress), 0200)
		}
	})

	// Step 5: Initialize network and create volume
	t.Log("Step 5: Initializing network...")
	err = networkMgr.Initialize(ctx, []string{})
	require.NoError(t, err)

	t.Log("Step 6: Setting up persistent volume for Ollama models...")
	vol, err := volumeMgr.GetVolumeByName(ctx, "ollama-models")
	if err != nil {
		vol, err = volumeMgr.CreateVolume(ctx, volumes.CreateVolumeRequest{
			Name:   "ollama-models",
			SizeGb: 5,
		})
		require.NoError(t, err)
		t.Logf("Created new volume: %s", vol.Name)
	} else {
		t.Logf("Using existing volume: %s", vol.Name)
	}

	// Step 7: Ensure system files
	t.Log("Step 7: Ensuring system files...")
	err = systemMgr.EnsureSystemFiles(ctx)
	require.NoError(t, err)

	// Step 8: Create instance with GPU
	t.Log("Step 8: Creating instance with GPU and custom CUDA image...")
	createCtx, createCancel := context.WithTimeout(ctx, 120*time.Second)
	defer createCancel()

	inst, err := instanceMgr.CreateInstance(createCtx, instances.CreateInstanceRequest{
		Name:        "gpu-inference-test",
		Image:       imageName,
		Size:        8 * 1024 * 1024 * 1024, // 8GB RAM for CUDA
		HotplugSize: 8 * 1024 * 1024 * 1024,
		OverlaySize: 10 * 1024 * 1024 * 1024,
		Vcpus:       4,
		Env: map[string]string{
			"OLLAMA_HOST":   "0.0.0.0",
			"OLLAMA_MODELS": "/data/models",
		},
		NetworkEnabled: true,
		Devices:        []string{"inference-gpu"},
		Volumes: []instances.VolumeAttachment{
			{VolumeID: vol.Id, MountPath: "/data/models", Readonly: false},
		},
	})
	require.NoError(t, err)
	t.Logf("Instance created: %s", inst.Id)

	t.Cleanup(func() {
		t.Log("Cleanup: Deleting instance...")
		instanceMgr.DeleteInstance(ctx, inst.Id)
	})

	// Step 9: Wait for instance
	t.Log("Step 9: Waiting for instance to be ready...")
	err = waitForInstanceReady(ctx, t, instanceMgr, inst.Id, 60*time.Second)
	require.NoError(t, err)

	actualInst, err := instanceMgr.GetInstance(ctx, inst.Id)
	require.NoError(t, err)

	dialer, err := hypervisor.NewVsockDialer(actualInst.HypervisorType, actualInst.VsockSocket, actualInst.VsockCID)
	require.NoError(t, err)

	// Step 10: Install NVIDIA driver via DKMS
	// hypeman's init auto-installs kernel headers, so DKMS can build modules
	t.Log("Step 10: Installing NVIDIA driver (DKMS will build kernel modules)...")
	installDriverCmd := `export DEBIAN_FRONTEND=noninteractive && \
		apt-get update && \
		apt-get install -y nvidia-driver-550-server && \
		modprobe nvidia`

	installCtx, installCancel := context.WithTimeout(ctx, 300*time.Second) // 5 min for DKMS build
	defer installCancel()

	var installStdout, installStderr inferenceOutputBuffer
	for i := 0; i < 30; i++ { // Retry until exec agent is ready
		installStdout = inferenceOutputBuffer{}
		installStderr = inferenceOutputBuffer{}
		_, err = guest.ExecIntoInstance(installCtx, dialer, guest.ExecOptions{
			Command: []string{"/bin/sh", "-c", installDriverCmd},
			Stdin:   nil,
			Stdout:  &installStdout,
			Stderr:  &installStderr,
			TTY:     false,
		})
		if err == nil {
			break
		}
		t.Logf("Waiting for exec agent... (%d/30)", i+1)
		time.Sleep(2 * time.Second)
	}
	require.NoError(t, err, "Failed to install NVIDIA driver: stdout=%s stderr=%s", installStdout.String(), installStderr.String())
	t.Logf("Driver installation output:\nstdout: %s\nstderr: %s", installStdout.String(), installStderr.String())

	// Verify nvidia-smi works
	t.Log("Verifying nvidia-smi...")
	smiCtx, smiCancel := context.WithTimeout(ctx, 30*time.Second)
	defer smiCancel()

	var smiStdout, smiStderr inferenceOutputBuffer
	_, err = guest.ExecIntoInstance(smiCtx, dialer, guest.ExecOptions{
		Command: []string{"nvidia-smi"},
		Stdin:   nil,
		Stdout:  &smiStdout,
		Stderr:  &smiStderr,
		TTY:     false,
	})
	require.NoError(t, err, "nvidia-smi failed: stderr=%s", smiStderr.String())
	t.Logf("nvidia-smi output:\n%s", smiStdout.String())

	// Step 12: Wait for Ollama server
	t.Log("Step 12: Waiting for Ollama server to be ready...")
	ollamaReady := false
	for i := 0; i < 60; i++ { // 60 seconds for CUDA init
		healthCtx, healthCancel := context.WithTimeout(ctx, 5*time.Second)
		var healthStdout, healthStderr inferenceOutputBuffer

		_, err = guest.ExecIntoInstance(healthCtx, dialer, guest.ExecOptions{
			Command: []string{"/bin/sh", "-c", "ollama list 2>&1"},
			Stdout:  &healthStdout,
			Stderr:  &healthStderr,
		})
		healthCancel()

		output := healthStdout.String()
		if err == nil && !strings.Contains(output, "could not connect") {
			t.Logf("Ollama is ready (attempt %d)", i+1)
			ollamaReady = true
			break
		}
		if i%10 == 0 {
			t.Logf("Waiting for Ollama (attempt %d/60)...", i+1)
		}
		time.Sleep(time.Second)
	}
	require.True(t, ollamaReady, "Ollama server should become ready")

	// Step 13: Check GPU detection
	t.Log("Step 13: Checking GPU detection...")
	gpuCheckCtx, gpuCheckCancel := context.WithTimeout(ctx, 10*time.Second)
	defer gpuCheckCancel()

	// Check nvidia-smi (should work now with CUDA image)
	var nvidiaSmiStdout, nvidiaSmiStderr inferenceOutputBuffer
	_, _ = guest.ExecIntoInstance(gpuCheckCtx, dialer, guest.ExecOptions{
		Command: []string{"/bin/sh", "-c", "nvidia-smi 2>&1 || echo 'nvidia-smi failed'"},
		Stdout:  &nvidiaSmiStdout,
		Stderr:  &nvidiaSmiStderr,
	})
	nvidiaSmiOutput := nvidiaSmiStdout.String()
	if strings.Contains(nvidiaSmiOutput, "NVIDIA-SMI") {
		t.Logf("✓ nvidia-smi works! GPU detected:\n%s", truncateHead(nvidiaSmiOutput, 500))
	} else {
		t.Logf("nvidia-smi output: %s", nvidiaSmiOutput)
	}

	// Check NVIDIA kernel modules
	var modulesStdout inferenceOutputBuffer
	guest.ExecIntoInstance(gpuCheckCtx, dialer, guest.ExecOptions{
		Command: []string{"/bin/sh", "-c", "cat /proc/modules | grep nvidia"},
		Stdout:  &modulesStdout,
	})
	if modulesStdout.String() != "" {
		t.Logf("✓ NVIDIA kernel modules loaded:\n%s", modulesStdout.String())
	}

	// Check device nodes
	var devStdout inferenceOutputBuffer
	guest.ExecIntoInstance(gpuCheckCtx, dialer, guest.ExecOptions{
		Command: []string{"/bin/sh", "-c", "ls -la /dev/nvidia* 2>&1"},
		Stdout:  &devStdout,
	})
	if !strings.Contains(devStdout.String(), "No such file") {
		t.Logf("✓ NVIDIA device nodes:\n%s", devStdout.String())
	}

	// Step 14: Pull model via exec (needed for first time)
	t.Log("Step 14: Ensuring TinyLlama model is available...")

	var listStdout inferenceOutputBuffer
	guest.ExecIntoInstance(gpuCheckCtx, dialer, guest.ExecOptions{
		Command: []string{"/bin/sh", "-c", "ollama list 2>&1"},
		Stdout:  &listStdout,
	})

	if !strings.Contains(listStdout.String(), "tinyllama") {
		t.Log("Model not cached - pulling now...")
		pullCtx, pullCancel := context.WithTimeout(ctx, 10*time.Minute)
		defer pullCancel()

		var pullStdout inferenceOutputBuffer
		_, pullErr := guest.ExecIntoInstance(pullCtx, dialer, guest.ExecOptions{
			Command: []string{"/bin/sh", "-c", "ollama pull tinyllama 2>&1"},
			Stdout:  &pullStdout,
		})
		t.Logf("Pull output: %s", truncateTail(pullStdout.String(), 500))
		require.NoError(t, pullErr, "ollama pull should succeed")
	} else {
		t.Log("Model already cached")
	}

	// Step 15: Test inference via HTTP API using the VM's private IP
	// This is much faster than using `ollama run` CLI
	t.Log("Step 15: Running inference via Ollama API...")
	require.NotEmpty(t, actualInst.IP, "Instance should have a private IP")
	ollamaURL := fmt.Sprintf("http://%s:11434/api/generate", actualInst.IP)
	t.Logf("Calling Ollama API at %s", ollamaURL)

	// Create the inference request
	inferenceReq := map[string]interface{}{
		"model":  "tinyllama",
		"prompt": "Say hello in 3 words",
		"stream": false,
	}
	reqBody, err := json.Marshal(inferenceReq)
	require.NoError(t, err)

	// Make the HTTP request with timeout
	httpClient := &http.Client{Timeout: 2 * time.Minute}
	start := time.Now()
	resp, err := httpClient.Post(ollamaURL, "application/json", bytes.NewReader(reqBody))
	elapsed := time.Since(start)

	if err != nil {
		// Log console for debugging
		consoleLogPath := p.InstanceAppLog(inst.Id)
		if consoleLog, readErr := os.ReadFile(consoleLogPath); readErr == nil {
			t.Logf("=== VM Console Log ===\n%s\n=== End ===", truncateTail(string(consoleLog), 3000))
		}
	}
	require.NoError(t, err, "HTTP request to Ollama should succeed")
	defer resp.Body.Close()

	require.Equal(t, http.StatusOK, resp.StatusCode, "Ollama should return 200")

	// Parse response
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)

	var ollamaResp struct {
		Response      string `json:"response"`
		Done          bool   `json:"done"`
		TotalDuration int64  `json:"total_duration"` // nanoseconds
		EvalDuration  int64  `json:"eval_duration"`  // nanoseconds
		EvalCount     int    `json:"eval_count"`     // tokens generated
	}
	err = json.Unmarshal(body, &ollamaResp)
	require.NoError(t, err)

	// Log results
	t.Logf("Inference response: %s", ollamaResp.Response)
	t.Logf("Total time: %v (API reported: %dms)", elapsed, ollamaResp.TotalDuration/1e6)
	if ollamaResp.EvalCount > 0 && ollamaResp.EvalDuration > 0 {
		tokensPerSec := float64(ollamaResp.EvalCount) / (float64(ollamaResp.EvalDuration) / 1e9)
		t.Logf("Generation speed: %.1f tokens/sec (%d tokens in %dms)",
			tokensPerSec, ollamaResp.EvalCount, ollamaResp.EvalDuration/1e6)
	}

	// Verify output
	assert.True(t, ollamaResp.Done, "Inference should complete")
	assert.NotEmpty(t, ollamaResp.Response, "Model should generate output")
	assert.True(t, len(ollamaResp.Response) > 5, "Model output should be substantive")

	// GPU inference should be fast (< 5 seconds for this small prompt)
	assert.Less(t, elapsed, 30*time.Second, "GPU inference should be fast")

	t.Log("✅ GPU inference test PASSED!")
}

// getDockerfilePath returns the path to the CUDA+Ollama Dockerfile
func getDockerfilePath(t *testing.T) string {
	_, thisFile, _, ok := runtime.Caller(0)
	require.True(t, ok, "Could not get current file path")
	return filepath.Join(filepath.Dir(thisFile), "testdata", "ollama-cuda", "Dockerfile")
}

// buildCustomCudaImage builds the custom CUDA+Ollama Docker image
func buildCustomCudaImage(t *testing.T, dockerfilePath, imageName string) {
	t.Helper()

	// Check if image already exists
	checkCmd := osExec.Command("docker", "image", "inspect", imageName)
	if checkCmd.Run() == nil {
		t.Logf("Docker image %s already exists, skipping build", imageName)
		return
	}

	t.Logf("Building Docker image %s (this may take several minutes)...", imageName)
	dockerfileDir := filepath.Dir(dockerfilePath)

	cmd := osExec.Command("docker", "build", "-t", imageName, dockerfileDir)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	err := cmd.Run()
	require.NoError(t, err, "Docker build should succeed")
	t.Logf("Docker image %s built successfully", imageName)
}

// pushLocalDockerImage loads an image from local Docker and pushes to hypeman's test registry
func pushLocalDockerImage(t *testing.T, dockerImage, serverHost string) {
	t.Helper()

	t.Log("Loading image from Docker daemon...")
	srcRef, err := name.ParseReference(dockerImage)
	require.NoError(t, err, "Parse source image reference")

	img, err := daemon.Image(srcRef)
	require.NoError(t, err, "Load image from Docker daemon")

	// Check image size for progress context
	layers, _ := img.Layers()
	var totalSize int64
	for _, layer := range layers {
		if size, err := layer.Size(); err == nil {
			totalSize += size
		}
	}
	t.Logf("Image has %d layers, ~%.1f GB total", len(layers), float64(totalSize)/1e9)

	// Push to test registry with a tag (not just digest) so ListImages can find it
	targetRef := fmt.Sprintf("%s/test/ollama-cuda:latest", serverHost)
	t.Logf("Pushing to %s", targetRef)

	dstRef, err := name.ParseReference(targetRef, name.Insecure)
	require.NoError(t, err, "Parse target reference")

	err = remote.Write(dstRef, img)
	require.NoError(t, err, "Push to registry")
}

// inferenceOutputBuffer is a simple buffer for capturing command output
type inferenceOutputBuffer struct {
	buf bytes.Buffer
}

func (b *inferenceOutputBuffer) Write(p []byte) (n int, err error) {
	return b.buf.Write(p)
}

func (b *inferenceOutputBuffer) String() string {
	return b.buf.String()
}

// truncateTail returns the last n characters of s
func truncateTail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "..." + s[len(s)-n:]
}

// truncateHead returns the first n characters of s
func truncateHead(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
