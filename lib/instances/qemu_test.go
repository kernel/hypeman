package instances

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/onkernel/hypeman/cmd/api/config"
	"github.com/onkernel/hypeman/lib/devices"
	"github.com/onkernel/hypeman/lib/guest"
	"github.com/onkernel/hypeman/lib/hypervisor"
	"github.com/onkernel/hypeman/lib/hypervisor/qemu"
	"github.com/onkernel/hypeman/lib/images"
	"github.com/onkernel/hypeman/lib/ingress"
	"github.com/onkernel/hypeman/lib/network"
	"github.com/onkernel/hypeman/lib/paths"
	"github.com/onkernel/hypeman/lib/system"
	"github.com/onkernel/hypeman/lib/volumes"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// setupTestManagerForQEMU creates a manager configured to use QEMU as the default hypervisor
func setupTestManagerForQEMU(t *testing.T) (*manager, string) {
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
	volumeManager := volumes.NewManager(p, 0, nil) // 0 = unlimited storage
	limits := ResourceLimits{
		MaxOverlaySize:       100 * 1024 * 1024 * 1024, // 100GB
		MaxVcpusPerInstance:  0,                        // unlimited
		MaxMemoryPerInstance: 0,                        // unlimited
		MaxTotalVcpus:        0,                        // unlimited
		MaxTotalMemory:       0,                        // unlimited
	}
	mgr := NewManager(p, imageManager, systemManager, networkManager, deviceManager, volumeManager, limits, hypervisor.TypeQEMU, nil, nil).(*manager)

	// Register cleanup to kill any orphaned QEMU processes
	t.Cleanup(func() {
		cleanupOrphanedQEMUProcesses(t, mgr)
	})

	return mgr, tmpDir
}

// cleanupOrphanedQEMUProcesses kills any QEMU processes from metadata
func cleanupOrphanedQEMUProcesses(t *testing.T, mgr *manager) {
	metaFiles, err := mgr.listMetadataFiles()
	if err != nil {
		return
	}

	for _, metaFile := range metaFiles {
		id := filepath.Base(filepath.Dir(metaFile))
		meta, err := mgr.loadMetadata(id)
		if err != nil {
			continue
		}

		if meta.HypervisorPID != nil {
			pid := *meta.HypervisorPID
			if err := syscall.Kill(pid, 0); err == nil {
				t.Logf("Cleaning up orphaned QEMU process: PID %d (instance %s)", pid, id)
				syscall.Kill(pid, syscall.SIGKILL)
				WaitForProcessExit(pid, 1*time.Second)
			}
		}
	}
}

// waitForQEMUReady polls QEMU status via QMP until it's running or times out
func waitForQEMUReady(ctx context.Context, socketPath string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)

	for time.Now().Before(deadline) {
		client, err := qemu.New(socketPath)
		if err != nil {
			time.Sleep(100 * time.Millisecond)
			continue
		}

		info, err := client.GetVMInfo(ctx)
		if err != nil {
			time.Sleep(100 * time.Millisecond)
			continue
		}

		if info.State == hypervisor.StateRunning {
			return nil
		}

		time.Sleep(100 * time.Millisecond)
	}

	return fmt.Errorf("QEMU VM did not reach running state within %v", timeout)
}

// collectQEMULogs gets the last N lines of logs (non-streaming)
func collectQEMULogs(ctx context.Context, mgr *manager, instanceID string, n int) (string, error) {
	logChan, err := mgr.StreamInstanceLogs(ctx, instanceID, n, false, LogSourceApp)
	if err != nil {
		return "", err
	}

	var lines []string
	for line := range logChan {
		lines = append(lines, line)
	}

	return strings.Join(lines, "\n"), nil
}

// qemuInstanceResolver is a simple resolver for ingress tests
type qemuInstanceResolver struct {
	ip     string
	exists bool
}

func (r *qemuInstanceResolver) ResolveInstanceIP(ctx context.Context, nameOrID string) (string, error) {
	if r.ip == "" {
		return "", fmt.Errorf("instance not found: %s", nameOrID)
	}
	return r.ip, nil
}

func (r *qemuInstanceResolver) InstanceExists(ctx context.Context, nameOrID string) (bool, error) {
	return r.exists, nil
}

func (r *qemuInstanceResolver) ResolveInstance(ctx context.Context, nameOrID string) (string, string, error) {
	if !r.exists {
		return "", "", fmt.Errorf("instance not found: %s", nameOrID)
	}
	return nameOrID, nameOrID, nil
}

// TestQEMUBasicEndToEnd tests the complete instance lifecycle with QEMU.
// This is the primary integration test for QEMU support.
// It tests: create, get, list, logs, network, ingress, volumes, exec, and delete.
// It does NOT test: snapshot/standby, hot memory resize (not supported by QEMU in first pass).
func TestQEMUBasicEndToEnd(t *testing.T) {
	// Require KVM access
	if _, err := os.Stat("/dev/kvm"); os.IsNotExist(err) {
		t.Fatal("/dev/kvm not available - ensure KVM is enabled and user is in 'kvm' group (sudo usermod -aG kvm $USER)")
	}

	// Require QEMU to be installed
	starter := qemu.NewStarter()
	if _, err := starter.GetBinaryPath(nil, ""); err != nil {
		t.Fatalf("QEMU not available: %v", err)
	}

	manager, tmpDir := setupTestManagerForQEMU(t)
	ctx := context.Background()

	// Get the image manager for image operations
	imageManager, err := images.NewManager(paths.New(tmpDir), 1, nil)
	require.NoError(t, err)

	// Pull nginx image
	t.Log("Pulling nginx:alpine image...")
	nginxImage, err := imageManager.CreateImage(ctx, images.CreateImageRequest{
		Name: "docker.io/library/nginx:alpine",
	})
	require.NoError(t, err)

	// Wait for image to be ready
	t.Log("Waiting for image build to complete...")
	imageName := nginxImage.Name
	for i := 0; i < 60; i++ {
		img, err := imageManager.GetImage(ctx, imageName)
		if err == nil && img.Status == images.StatusReady {
			nginxImage = img
			break
		}
		if err == nil && img.Status == images.StatusFailed {
			t.Fatalf("Image build failed: %s", *img.Error)
		}
		time.Sleep(1 * time.Second)
	}
	require.Equal(t, images.StatusReady, nginxImage.Status, "Image should be ready after 60 seconds")
	t.Log("Nginx image ready")

	// Ensure system files
	systemManager := system.NewManager(paths.New(tmpDir))
	t.Log("Ensuring system files (downloads kernel and builds initrd)...")
	err = systemManager.EnsureSystemFiles(ctx)
	require.NoError(t, err)
	t.Log("System files ready")

	// Create a volume to attach
	p := paths.New(tmpDir)
	volumeManager := volumes.NewManager(p, 0, nil)
	t.Log("Creating volume...")
	vol, err := volumeManager.CreateVolume(ctx, volumes.CreateVolumeRequest{
		Name:   "test-data",
		SizeGb: 1,
	})
	require.NoError(t, err)
	require.NotNil(t, vol)
	t.Logf("Volume created: %s", vol.Id)

	// Verify volume file exists and is not attached
	assert.FileExists(t, p.VolumeData(vol.Id))
	assert.Empty(t, vol.Attachments, "Volume should not be attached yet")

	// Initialize network
	networkManager := network.NewManager(p, &config.Config{
		DataDir:    tmpDir,
		BridgeName: "vmbr0",
		SubnetCIDR: "10.100.0.0/16",
		DNSServer:  "1.1.1.1",
	}, nil)
	t.Log("Initializing network...")
	err = networkManager.Initialize(ctx, nil)
	require.NoError(t, err)
	t.Log("Network initialized")

	// Create instance with QEMU hypervisor
	req := CreateInstanceRequest{
		Name:           "test-nginx-qemu",
		Image:          "docker.io/library/nginx:alpine",
		Size:           2 * 1024 * 1024 * 1024,  // 2GB
		HotplugSize:    512 * 1024 * 1024,       // 512MB (unused by QEMU, but part of the request)
		OverlaySize:    10 * 1024 * 1024 * 1024, // 10GB
		Vcpus:          1,
		NetworkEnabled: true,
		Hypervisor:     hypervisor.TypeQEMU, // Explicitly use QEMU
		Env: map[string]string{
			"TEST_VAR": "test_value",
		},
		Volumes: []VolumeAttachment{
			{
				VolumeID:  vol.Id,
				MountPath: "/mnt/data",
				Readonly:  false,
			},
		},
	}

	t.Log("Creating QEMU instance...")
	inst, err := manager.CreateInstance(ctx, req)
	require.NoError(t, err)
	require.NotNil(t, inst)
	t.Logf("Instance created: %s (hypervisor: %s)", inst.Id, inst.HypervisorType)

	// Verify instance fields
	assert.NotEmpty(t, inst.Id)
	assert.Equal(t, "test-nginx-qemu", inst.Name)
	assert.Equal(t, "docker.io/library/nginx:alpine", inst.Image)
	assert.Equal(t, StateRunning, inst.State)
	assert.Equal(t, hypervisor.TypeQEMU, inst.HypervisorType)
	assert.False(t, inst.HasSnapshot)
	assert.NotEmpty(t, inst.KernelVersion)

	// Verify volume is attached to instance
	assert.Len(t, inst.Volumes, 1, "Instance should have 1 volume attached")
	assert.Equal(t, vol.Id, inst.Volumes[0].VolumeID)
	assert.Equal(t, "/mnt/data", inst.Volumes[0].MountPath)

	// Verify volume shows as attached
	vol, err = volumeManager.GetVolume(ctx, vol.Id)
	require.NoError(t, err)
	require.Len(t, vol.Attachments, 1, "Volume should be attached")
	assert.Equal(t, inst.Id, vol.Attachments[0].InstanceID)
	assert.Equal(t, "/mnt/data", vol.Attachments[0].MountPath)

	// Verify directories exist
	assert.DirExists(t, p.InstanceDir(inst.Id))
	assert.FileExists(t, p.InstanceMetadata(inst.Id))
	assert.FileExists(t, p.InstanceOverlay(inst.Id))
	assert.FileExists(t, p.InstanceConfigDisk(inst.Id))

	// Wait for VM to be fully running
	err = waitForQEMUReady(ctx, inst.SocketPath, 10*time.Second)
	require.NoError(t, err, "QEMU VM should reach running state")

	// Get instance
	retrieved, err := manager.GetInstance(ctx, inst.Id)
	require.NoError(t, err)
	assert.Equal(t, inst.Id, retrieved.Id)
	assert.Equal(t, StateRunning, retrieved.State)

	// List instances
	instances, err := manager.ListInstances(ctx)
	require.NoError(t, err)
	assert.Len(t, instances, 1)
	assert.Equal(t, inst.Id, instances[0].Id)

	// Poll for logs to contain nginx startup message
	var logs string
	foundNginxStartup := false
	for i := 0; i < 50; i++ {
		logs, err = collectQEMULogs(ctx, manager, inst.Id, 100)
		require.NoError(t, err)

		if strings.Contains(logs, "start worker processes") {
			foundNginxStartup = true
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	t.Logf("Instance logs (last 100 lines):\n%s", logs)
	assert.True(t, foundNginxStartup, "Nginx should have started worker processes within 5 seconds")

	// Test ingress - route external traffic to nginx
	t.Log("Testing ingress routing to nginx...")

	// Get random free ports
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	ingressPort := listener.Addr().(*net.TCPAddr).Port
	listener.Close()

	adminListener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	adminPort := adminListener.Addr().(*net.TCPAddr).Port
	adminListener.Close()

	t.Logf("Using random ports: ingress=%d, admin=%d", ingressPort, adminPort)

	// Create ingress manager
	ingressConfig := ingress.Config{
		ListenAddress:  "127.0.0.1",
		AdminAddress:   "127.0.0.1",
		AdminPort:      adminPort,
		DNSPort:        0,
		StopOnShutdown: true,
	}

	instanceIP := inst.IP
	require.NotEmpty(t, instanceIP, "Instance should have an IP address")
	t.Logf("Instance IP: %s", instanceIP)

	resolver := &qemuInstanceResolver{
		ip:     instanceIP,
		exists: true,
	}

	ingressManager := ingress.NewManager(p, ingressConfig, resolver, nil)

	// Initialize ingress manager (starts Caddy)
	t.Log("Starting Caddy...")
	err = ingressManager.Initialize(ctx)
	require.NoError(t, err, "Ingress manager should initialize successfully")

	t.Cleanup(func() {
		t.Log("Shutting down Caddy...")
		if err := ingressManager.Shutdown(context.Background()); err != nil {
			t.Logf("Warning: failed to shutdown ingress manager: %v", err)
		}
	})

	// Create an ingress rule
	t.Log("Creating ingress rule...")
	ingressReq := ingress.CreateIngressRequest{
		Name: "test-nginx-ingress",
		Rules: []ingress.IngressRule{
			{
				Match: ingress.IngressMatch{
					Hostname: "test.local",
					Port:     ingressPort,
				},
				Target: ingress.IngressTarget{
					Instance: "test-nginx-qemu",
					Port:     80,
				},
			},
		},
	}
	ing, err := ingressManager.Create(ctx, ingressReq)
	require.NoError(t, err)
	require.NotNil(t, ing)
	t.Logf("Ingress created: %s", ing.ID)

	// Make HTTP request through Caddy to nginx
	t.Log("Making HTTP request through Caddy to nginx...")
	client := &http.Client{Timeout: 2 * time.Second}
	var resp *http.Response
	var lastErr error
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		httpReq, err := http.NewRequest("GET", fmt.Sprintf("http://127.0.0.1:%d/", ingressPort), nil)
		require.NoError(t, err)
		httpReq.Host = "test.local"

		resp, lastErr = client.Do(httpReq)
		if lastErr == nil && resp.StatusCode == http.StatusOK {
			break
		}
		if resp != nil {
			resp.Body.Close()
			resp = nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	require.NoError(t, lastErr, "HTTP request through Caddy should succeed")
	require.NotNil(t, resp, "HTTP response should not be nil")
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode, "Should get 200 OK from nginx")

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	assert.Contains(t, string(body), "nginx", "Response should contain nginx welcome page")
	t.Logf("Got response from nginx through Caddy: %d bytes", len(body))

	err = ingressManager.Delete(ctx, ing.ID)
	require.NoError(t, err)
	t.Log("Ingress deleted")

	// Test volume is accessible from inside the guest via exec
	t.Log("Testing volume from inside guest via exec...")

	runCmd := func(command ...string) (string, int, error) {
		var lastOutput string
		var lastExitCode int
		var lastErr error

		dialer, err := hypervisor.NewVsockDialer(inst.HypervisorType, inst.VsockSocket, inst.VsockCID)
		if err != nil {
			return "", -1, err
		}

		for attempt := 0; attempt < 5; attempt++ {
			if attempt > 0 {
				time.Sleep(200 * time.Millisecond)
			}

			var stdout, stderr bytes.Buffer
			exit, err := guest.ExecIntoInstance(ctx, dialer, guest.ExecOptions{
				Command: command,
				Stdout:  &stdout,
				Stderr:  &stderr,
				TTY:     false,
			})

			output := stdout.String()
			if stderr.Len() > 0 {
				output += stderr.String()
			}
			output = strings.TrimSpace(output)

			if err != nil {
				lastErr = err
				lastOutput = output
				lastExitCode = -1
				continue
			}

			lastOutput = output
			lastExitCode = exit.Code
			lastErr = nil

			if output != "" || exit.Code == 0 {
				return output, exit.Code, nil
			}
		}

		return lastOutput, lastExitCode, lastErr
	}

	// Test volume in a single exec call
	testContent := "hello-from-qemu-volume-test"
	script := fmt.Sprintf(`
		set -e
		echo "=== Volume directory ==="
		ls -la /mnt/data
		echo "=== Writing test file ==="
		echo '%s' > /mnt/data/test.txt
		echo "=== Reading test file ==="
		cat /mnt/data/test.txt
		echo "=== Volume mount info ==="
		df -h /mnt/data
	`, testContent)

	output, exitCode, err := runCmd("sh", "-c", script)
	require.NoError(t, err, "Volume test script should execute")
	require.Equal(t, 0, exitCode, "Volume test script should succeed")

	require.Contains(t, output, "lost+found", "Volume should be ext4-formatted")
	require.Contains(t, output, testContent, "Should be able to read written content")
	require.Contains(t, output, "/dev/vd", "Volume should be mounted from block device")
	t.Logf("Volume test output:\n%s", output)
	t.Log("Volume read/write test passed!")

	// Delete instance
	t.Log("Deleting instance...")
	err = manager.DeleteInstance(ctx, inst.Id)
	require.NoError(t, err)

	// Verify cleanup
	assert.NoDirExists(t, p.InstanceDir(inst.Id))

	// Verify instance no longer exists
	_, err = manager.GetInstance(ctx, inst.Id)
	assert.ErrorIs(t, err, ErrNotFound)

	// Verify volume is detached but still exists
	vol, err = volumeManager.GetVolume(ctx, vol.Id)
	require.NoError(t, err)
	assert.Empty(t, vol.Attachments, "Volume should be detached after instance deletion")
	assert.FileExists(t, p.VolumeData(vol.Id), "Volume file should still exist")

	// Delete volume
	t.Log("Deleting volume...")
	err = volumeManager.DeleteVolume(ctx, vol.Id)
	require.NoError(t, err)

	// Verify volume is gone
	_, err = volumeManager.GetVolume(ctx, vol.Id)
	assert.ErrorIs(t, err, volumes.ErrNotFound)

	t.Log("QEMU instance lifecycle test complete!")
}
