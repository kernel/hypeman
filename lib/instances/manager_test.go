package instances

import (
	"bytes"
	"context"
	"crypto/tls"
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

	"github.com/joho/godotenv"
	"github.com/kernel/hypeman/cmd/api/config"
	"github.com/kernel/hypeman/lib/devices"
	"github.com/kernel/hypeman/lib/guest"
	"github.com/kernel/hypeman/lib/hypervisor"
	"github.com/kernel/hypeman/lib/images"
	"github.com/kernel/hypeman/lib/ingress"
	"github.com/kernel/hypeman/lib/network"
	"github.com/kernel/hypeman/lib/paths"
	"github.com/kernel/hypeman/lib/resources"
	"github.com/kernel/hypeman/lib/system"
	"github.com/kernel/hypeman/lib/vmm"
	"github.com/kernel/hypeman/lib/volumes"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// setupTestManager creates a manager and registers cleanup for any orphaned processes
func setupTestManager(t *testing.T) (*manager, string) {
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
	}
	mgr := NewManager(p, imageManager, systemManager, networkManager, deviceManager, volumeManager, limits, "", nil, nil).(*manager)

	// Set up resource validation using the real ResourceManager
	resourceMgr := resources.NewManager(cfg, p)
	resourceMgr.SetInstanceLister(mgr)
	resourceMgr.SetImageLister(imageManager)
	resourceMgr.SetVolumeLister(volumeManager)
	err = resourceMgr.Initialize(context.Background())
	require.NoError(t, err)
	mgr.SetResourceValidator(resourceMgr)

	// Register cleanup to kill any orphaned Cloud Hypervisor processes
	t.Cleanup(func() {
		cleanupOrphanedProcesses(t, mgr)
	})

	return mgr, tmpDir
}

// waitForVMReady polls VM state via VMM API until it's running or times out
func waitForVMReady(ctx context.Context, socketPath string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)

	for time.Now().Before(deadline) {
		// Try to connect to VMM
		client, err := vmm.NewVMM(socketPath)
		if err != nil {
			// Socket might not be ready yet
			time.Sleep(100 * time.Millisecond)
			continue
		}

		// Get VM info
		infoResp, err := client.GetVmInfoWithResponse(ctx)
		if err != nil {
			time.Sleep(100 * time.Millisecond)
			continue
		}

		if infoResp.StatusCode() != 200 || infoResp.JSON200 == nil {
			time.Sleep(100 * time.Millisecond)
			continue
		}

		// Check if VM is running
		if infoResp.JSON200.State == vmm.Running {
			return nil
		}

		time.Sleep(100 * time.Millisecond)
	}

	return fmt.Errorf("VM did not reach running state within %v", timeout)
}

// waitForLogMessage polls instance logs until the message appears or times out
func waitForLogMessage(ctx context.Context, mgr *manager, instanceID, message string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)

	for time.Now().Before(deadline) {
		logs, err := collectLogs(ctx, mgr, instanceID, 200)
		if err != nil {
			time.Sleep(100 * time.Millisecond)
			continue
		}

		if strings.Contains(logs, message) {
			return nil
		}

		time.Sleep(100 * time.Millisecond)
	}

	return fmt.Errorf("message %q not found in logs within %v", message, timeout)
}

// collectLogs gets the last N lines of logs (non-streaming)
func collectLogs(ctx context.Context, mgr *manager, instanceID string, n int) (string, error) {
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

// cleanupOrphanedProcesses kills any Cloud Hypervisor processes from metadata
func cleanupOrphanedProcesses(t *testing.T, mgr *manager) {
	// Find all metadata files
	metaFiles, err := mgr.listMetadataFiles()
	if err != nil {
		return // No metadata files, nothing to clean
	}

	for _, metaFile := range metaFiles {
		// Extract instance ID from path
		id := filepath.Base(filepath.Dir(metaFile))

		// Load metadata
		meta, err := mgr.loadMetadata(id)
		if err != nil {
			continue
		}

		// If metadata has a PID, try to kill it
		if meta.HypervisorPID != nil {
			pid := *meta.HypervisorPID

			// Check if process exists
			if err := syscall.Kill(pid, 0); err == nil {
				t.Logf("Cleaning up orphaned hypervisor process: PID %d (instance %s)", pid, id)
				syscall.Kill(pid, syscall.SIGKILL)

				// Wait for process to exit
				WaitForProcessExit(pid, 1*time.Second)
			}
		}
	}
}

func TestBasicEndToEnd(t *testing.T) {
	// Require KVM access (don't skip, fail informatively)
	if _, err := os.Stat("/dev/kvm"); os.IsNotExist(err) {
		t.Skip("/dev/kvm not available, skipping on this platform")
	}

	manager, tmpDir := setupTestManager(t) // Automatically registers cleanup
	ctx := context.Background()

	// Get the image manager from the manager (we need it for image operations)
	imageManager, err := images.NewManager(paths.New(tmpDir), 1, nil)
	require.NoError(t, err)

	// Pull nginx image (runs a daemon, won't exit)
	t.Log("Pulling nginx:alpine image...")
	nginxImage, err := imageManager.CreateImage(ctx, images.CreateImageRequest{
		Name: "docker.io/library/nginx:alpine",
	})
	require.NoError(t, err)

	// Wait for image to be ready (poll by name)
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
	t.Log("Ensuring system files (downloads kernel ~70MB and builds initrd ~1MB)...")
	err = systemManager.EnsureSystemFiles(ctx)
	require.NoError(t, err)
	t.Log("System files ready")

	// Create a volume to attach
	p := paths.New(tmpDir)
	volumeManager := volumes.NewManager(p, 0, nil) // 0 = unlimited storage
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

	// Initialize network for ingress testing
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

	// Create instance with real nginx image and attached volume
	req := CreateInstanceRequest{
		Name:           "test-nginx",
		Image:          "docker.io/library/nginx:alpine",
		Size:           2 * 1024 * 1024 * 1024,  // 2GB (needs extra room for initrd with NVIDIA libs)
		HotplugSize:    512 * 1024 * 1024,       // 512MB
		OverlaySize:    10 * 1024 * 1024 * 1024, // 10GB
		Vcpus:          1,
		NetworkEnabled: true, // Enable network for ingress test
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

	t.Log("Creating instance...")
	inst, err := manager.CreateInstance(ctx, req)
	require.NoError(t, err)
	require.NotNil(t, inst)
	t.Logf("Instance created: %s", inst.Id)

	// Verify instance fields
	assert.NotEmpty(t, inst.Id)
	assert.Equal(t, "test-nginx", inst.Name)
	assert.Equal(t, "docker.io/library/nginx:alpine", inst.Image)
	assert.Equal(t, StateRunning, inst.State)
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
	err = waitForVMReady(ctx, inst.SocketPath, 5*time.Second)
	require.NoError(t, err, "VM should reach running state")

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
	for i := 0; i < 50; i++ { // Poll for up to 5 seconds (50 * 100ms)
		logs, err = collectLogs(ctx, manager, inst.Id, 100)
		require.NoError(t, err)

		if strings.Contains(logs, "start worker processes") {
			foundNginxStartup = true
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	t.Logf("Instance logs (last 100 lines):\n%s", logs)

	// Verify nginx started successfully
	assert.True(t, foundNginxStartup, "Nginx should have started worker processes within 5 seconds")

	// Test ingress - route external traffic to nginx through Caddy
	t.Log("Testing ingress routing to nginx...")

	// Get random free ports for Caddy
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	ingressPort := listener.Addr().(*net.TCPAddr).Port
	listener.Close()

	adminListener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	adminPort := adminListener.Addr().(*net.TCPAddr).Port
	adminListener.Close()

	t.Logf("Using random ports: ingress=%d, admin=%d", ingressPort, adminPort)

	// Create ingress manager with random ports
	ingressConfig := ingress.Config{
		ListenAddress:  "127.0.0.1",
		AdminAddress:   "127.0.0.1",
		AdminPort:      adminPort,
		DNSPort:        0, // Use random port for testing
		StopOnShutdown: true,
	}

	// Create a simple instance resolver that returns the instance IP
	instanceIP := inst.IP
	require.NotEmpty(t, instanceIP, "Instance should have an IP address")
	t.Logf("Instance IP: %s", instanceIP)

	resolver := &testInstanceResolver{
		ip:     instanceIP,
		exists: true,
	}

	// Pass nil for otelLogger - no log forwarding in tests
	ingressManager := ingress.NewManager(p, ingressConfig, resolver, nil)

	// Initialize ingress manager (starts Caddy)
	t.Log("Starting Caddy...")
	err = ingressManager.Initialize(ctx)
	require.NoError(t, err, "Ingress manager should initialize successfully")

	// Ensure we clean up Caddy - use t.Cleanup for guaranteed cleanup even on test failures
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
					Instance: "test-nginx",
					Port:     80,
				},
			},
		},
	}
	ing, err := ingressManager.Create(ctx, ingressReq)
	require.NoError(t, err)
	require.NotNil(t, ing)
	t.Logf("Ingress created: %s", ing.ID)

	// Make HTTP request through Caddy to nginx with retry
	// Caddy reloads config dynamically via the admin API
	t.Log("Making HTTP request through Caddy to nginx...")
	client := &http.Client{Timeout: 2 * time.Second}
	var resp *http.Response
	var lastErr error
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		req, err := http.NewRequest("GET", fmt.Sprintf("http://127.0.0.1:%d/", ingressPort), nil)
		require.NoError(t, err)
		req.Host = "test.local" // Set Host header to match ingress rule

		resp, lastErr = client.Do(req)
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

	// Verify we got a successful response from nginx
	assert.Equal(t, http.StatusOK, resp.StatusCode, "Should get 200 OK from nginx")

	// Read response body
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	assert.Contains(t, string(body), "nginx", "Response should contain nginx welcome page")
	t.Logf("Got response from nginx through Caddy: %d bytes", len(body))
	err = ingressManager.Delete(ctx, ing.ID)
	require.NoError(t, err)
	t.Log("Ingress deleted")

	// Test TLS ingress (only if ACME is configured via environment variables or .env file)
	// Try to load .env file from repository root (for local development)
	cwd, _ := os.Getwd()
	for dir := cwd; dir != "/"; dir = filepath.Dir(dir) {
		envFile := filepath.Join(dir, ".env")
		if _, err := os.Stat(envFile); err == nil {
			_ = godotenv.Load(envFile)
			t.Logf("Loaded .env from %s", envFile)
			break
		}
	}

	acmeEmail := os.Getenv("ACME_EMAIL")
	acmeDNSProvider := os.Getenv("ACME_DNS_PROVIDER")
	cloudflareToken := os.Getenv("CLOUDFLARE_API_TOKEN")
	tlsTestDomain := os.Getenv("TLS_TEST_DOMAIN")
	acmeCA := os.Getenv("ACME_CA")

	if acmeEmail != "" && acmeDNSProvider == "cloudflare" && cloudflareToken != "" && tlsTestDomain != "" {
		t.Log("Testing TLS ingress (ACME configured)...")

		// Get random port for HTTPS
		httpsListener, err := net.Listen("tcp", "0.0.0.0:0")
		require.NoError(t, err)
		httpsPort := httpsListener.Addr().(*net.TCPAddr).Port
		httpsListener.Close()

		// Get random port for TLS admin API
		tlsAdminListener, err := net.Listen("tcp", "127.0.0.1:0")
		require.NoError(t, err)
		tlsAdminPort := tlsAdminListener.Addr().(*net.TCPAddr).Port
		tlsAdminListener.Close()

		t.Logf("Using random ports for TLS test: https=%d, admin=%d", httpsPort, tlsAdminPort)

		// Create a new ingress manager with ACME configuration
		tlsIngressConfig := ingress.Config{
			ListenAddress:  "0.0.0.0", // Must be accessible for certificate validation
			AdminAddress:   "127.0.0.1",
			AdminPort:      tlsAdminPort,
			DNSPort:        0, // Use random port for testing
			StopOnShutdown: true,
			ACME: ingress.ACMEConfig{
				Email:              acmeEmail,
				DNSProvider:        ingress.DNSProviderCloudflare,
				CA:                 acmeCA, // Use staging CA if set, otherwise production
				CloudflareAPIToken: cloudflareToken,
				AllowedDomains:     tlsTestDomain, // Allow the test domain
			},
		}

		tlsIngressManager := ingress.NewManager(p, tlsIngressConfig, resolver, nil)

		// Initialize TLS ingress manager (starts a new Caddy instance)
		t.Log("Starting Caddy with TLS support...")
		err = tlsIngressManager.Initialize(ctx)
		require.NoError(t, err, "TLS ingress manager should initialize successfully")

		// Use t.Cleanup for guaranteed cleanup even on test failures
		t.Cleanup(func() {
			t.Log("Shutting down TLS Caddy...")
			if err := tlsIngressManager.Shutdown(context.Background()); err != nil {
				t.Logf("Warning: failed to shutdown TLS ingress manager: %v", err)
			}
		})

		// Create TLS ingress rule
		t.Logf("Creating TLS ingress rule for %s...", tlsTestDomain)
		tlsIngressReq := ingress.CreateIngressRequest{
			Name: "test-nginx-tls",
			Rules: []ingress.IngressRule{
				{
					Match: ingress.IngressMatch{
						Hostname: tlsTestDomain,
						Port:     httpsPort,
					},
					Target: ingress.IngressTarget{
						Instance: "test-nginx",
						Port:     80,
					},
					TLS:          true,
					RedirectHTTP: false, // Don't redirect, just test HTTPS
				},
			},
		}

		tlsIng, err := tlsIngressManager.Create(ctx, tlsIngressReq)
		require.NoError(t, err)
		require.NotNil(t, tlsIng)
		t.Logf("TLS Ingress created: %s", tlsIng.ID)

		// Wait for certificate to be issued (this can take 10-60 seconds with DNS-01)
		// Caddy will automatically obtain the certificate when the first request comes in
		t.Log("Making HTTPS request (certificate will be obtained on first request)...")

		// Create HTTP client that trusts the staging CA (or skips verification for testing)
		// ServerName sets the SNI (Server Name Indication) for the TLS handshake.
		// This is required because we connect to 127.0.0.1 but Caddy needs to know
		// which certificate to serve based on the hostname.
		tlsClient := &http.Client{
			Timeout: 90 * time.Second, // Long timeout for certificate issuance
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{
					InsecureSkipVerify: true,          // Accept staging CA certs
					ServerName:         tlsTestDomain, // Set SNI to match the certificate
				},
			},
		}

		var tlsResp *http.Response
		var tlsLastErr error
		tlsDeadline := time.Now().Add(90 * time.Second) // Allow up to 90s for cert issuance

		for time.Now().Before(tlsDeadline) {
			tlsReq, err := http.NewRequest("GET", fmt.Sprintf("https://127.0.0.1:%d/", httpsPort), nil)
			require.NoError(t, err)
			tlsReq.Host = tlsTestDomain // Set Host header to match ingress rule

			tlsResp, tlsLastErr = tlsClient.Do(tlsReq)
			if tlsLastErr == nil && tlsResp.StatusCode == http.StatusOK {
				break
			}
			if tlsResp != nil {
				tlsResp.Body.Close()
				tlsResp = nil
			}
			t.Logf("TLS request attempt failed: %v (retrying...)", tlsLastErr)
			time.Sleep(2 * time.Second)
		}

		require.NoError(t, tlsLastErr, "HTTPS request through Caddy should succeed")
		require.NotNil(t, tlsResp, "HTTPS response should not be nil")
		defer tlsResp.Body.Close()

		// Verify we got a successful response from nginx over HTTPS
		assert.Equal(t, http.StatusOK, tlsResp.StatusCode, "Should get 200 OK from nginx over HTTPS")

		// Read response body
		tlsBody, err := io.ReadAll(tlsResp.Body)
		require.NoError(t, err)
		assert.Contains(t, string(tlsBody), "nginx", "HTTPS response should contain nginx welcome page")
		t.Logf("Got HTTPS response from nginx through Caddy: %d bytes", len(tlsBody))

		// Clean up TLS ingress
		err = tlsIngressManager.Delete(ctx, tlsIng.ID)
		require.NoError(t, err)
		t.Log("TLS Ingress deleted")
	} else {
		t.Log("Skipping TLS ingress test (ACME not configured). Set ACME_EMAIL, ACME_DNS_PROVIDER=cloudflare, CLOUDFLARE_API_TOKEN, and TLS_TEST_DOMAIN to enable.")
	}

	// Test volume is accessible from inside the guest via exec
	t.Log("Testing volume from inside guest via exec...")

	// Helper to run command in guest with retry (exec agent may need time between connections)
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

			// Combine stdout and stderr
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

			// Success if we got output or it's a command expected to have no output
			if output != "" || exit.Code == 0 {
				return output, exit.Code, nil
			}
		}

		return lastOutput, lastExitCode, lastErr
	}

	// Test volume in a single exec call to avoid vsock connection issues
	// This verifies: mount exists, can write, can read back, is a real block device
	testContent := "hello-from-volume-test"
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

	// Verify all expected output is present
	require.Contains(t, output, "lost+found", "Volume should be ext4-formatted")
	require.Contains(t, output, testContent, "Should be able to read written content")
	require.Contains(t, output, "/dev/vd", "Volume should be mounted from block device")
	t.Logf("Volume test output:\n%s", output)
	t.Log("Volume read/write test passed!")

	// Test environment variables are accessible via exec (tests guest-agent has env vars)
	t.Log("Testing environment variables via exec...")
	output, exitCode, err = runCmd("printenv", "TEST_VAR")
	require.NoError(t, err, "printenv should execute")
	require.Equal(t, 0, exitCode, "printenv should succeed")
	assert.Equal(t, "test_value", strings.TrimSpace(output), "Environment variable should be accessible via exec")
	t.Log("Environment variable accessible via exec!")

	// Test streaming logs with live updates
	t.Log("Testing log streaming with live updates...")
	streamCtx, streamCancel := context.WithCancel(ctx)
	defer streamCancel()

	logChan, err := manager.StreamInstanceLogs(streamCtx, inst.Id, 10, true, LogSourceApp)
	require.NoError(t, err)

	// Create unique marker
	marker := fmt.Sprintf("STREAM_TEST_MARKER_%d", time.Now().UnixNano())

	// Start collecting lines and looking for marker
	markerFound := make(chan struct{})
	var streamedLines []string
	go func() {
		for line := range logChan {
			streamedLines = append(streamedLines, line)
			if strings.Contains(line, marker) {
				close(markerFound)
				return
			}
		}
	}()

	// Append marker to console log file
	consoleLogPath := p.InstanceAppLog(inst.Id)
	f, err := os.OpenFile(consoleLogPath, os.O_APPEND|os.O_WRONLY, 0644)
	require.NoError(t, err)
	_, err = fmt.Fprintln(f, marker)
	require.NoError(t, err)
	f.Close()

	// Wait for marker to appear in stream
	select {
	case <-markerFound:
		t.Logf("Successfully received live update through stream (collected %d lines)", len(streamedLines))
	case <-time.After(3 * time.Second):
		streamCancel()
		t.Fatalf("Timeout waiting for marker in stream (collected %d lines)", len(streamedLines))
	}
	streamCancel()

	// Test graceful stop: StopInstance sends Shutdown RPC -> init forwards SIGTERM
	// -> app exits -> init writes exit sentinel -> reboot(POWER_OFF) -> VM stops cleanly
	t.Log("Testing graceful stop via StopInstance...")
	stoppedInst, err := manager.StopInstance(ctx, inst.Id)
	require.NoError(t, err, "StopInstance should succeed")
	assert.Equal(t, StateStopped, stoppedInst.State, "Instance should be in Stopped state after StopInstance")

	// Verify the instance reports Stopped on subsequent query and exit info is populated
	retrieved, err = manager.GetInstance(ctx, inst.Id)
	require.NoError(t, err)
	assert.Equal(t, StateStopped, retrieved.State, "Instance should remain Stopped")
	require.NotNil(t, retrieved.ExitCode, "ExitCode should be populated after stop")
	t.Logf("Exit code after graceful stop: %d, message: %q", *retrieved.ExitCode, retrieved.ExitMessage)

	t.Log("Graceful stop test passed!")

	// Test restart: StartInstance should clear stale exit info and boot the VM
	t.Log("Testing restart after stop...")
	restartedInst, err := manager.StartInstance(ctx, inst.Id, StartInstanceRequest{})
	require.NoError(t, err, "StartInstance should succeed")
	assert.Equal(t, StateRunning, restartedInst.State, "Instance should be Running after restart")

	// Verify exit info was cleared
	retrieved, err = manager.GetInstance(ctx, inst.Id)
	require.NoError(t, err)
	assert.Nil(t, retrieved.ExitCode, "ExitCode should be nil after restart (stale exit info cleared)")
	assert.Empty(t, retrieved.ExitMessage, "ExitMessage should be empty after restart")
	t.Log("Restart test passed -- exit info cleared!")

	// Stop again before deleting
	_, err = manager.StopInstance(ctx, inst.Id)
	require.NoError(t, err)

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

	t.Log("Instance and volume lifecycle test complete!")
}

// TestAppExitPropagation verifies the full exit info pipeline when an app exits on its own:
// app exits -> init writes HYPEMAN-EXIT sentinel -> reboot(POWER_OFF) -> VM stops ->
// host lazily parses sentinel from serial log -> ExitCode/ExitMessage in metadata.
// Uses alpine with a non-existent command override to get exit code 127 ("command not found").
func TestAppExitPropagation(t *testing.T) {
	if _, err := os.Stat("/dev/kvm"); os.IsNotExist(err) {
		t.Skip("/dev/kvm not available, skipping on this platform")
	}

	manager, tmpDir := setupTestManager(t)
	ctx := context.Background()
	p := paths.New(tmpDir)

	imageManager, err := images.NewManager(p, 1, nil)
	require.NoError(t, err)

	t.Log("Pulling alpine:latest image...")
	alpineImage, err := imageManager.CreateImage(ctx, images.CreateImageRequest{
		Name: "docker.io/library/alpine:latest",
	})
	require.NoError(t, err)

	// Wait for image to be ready
	imageName := alpineImage.Name
	for i := 0; i < 60; i++ {
		img, err := imageManager.GetImage(ctx, imageName)
		if err == nil && img.Status == images.StatusReady {
			alpineImage = img
			break
		}
		if err == nil && img.Status == images.StatusFailed {
			t.Fatalf("Image build failed: %s", *img.Error)
		}
		time.Sleep(1 * time.Second)
	}
	require.Equal(t, images.StatusReady, alpineImage.Status)
	t.Log("Alpine image ready")

	// Ensure system files
	systemManager := system.NewManager(p)
	err = systemManager.EnsureSystemFiles(ctx)
	require.NoError(t, err)

	// Create instance with a non-existent command (like `docker run alpine /nonexistent`).
	// This overrides alpine's default CMD ("/bin/sh") with a command that doesn't exist,
	// causing exit code 127 ("command not found").
	inst, err := manager.CreateInstance(ctx, CreateInstanceRequest{
		Name:        "test-exit-propagation",
		Image:       "docker.io/library/alpine:latest",
		Size:        512 * 1024 * 1024, // 512MB
		HotplugSize: 0,
		OverlaySize: 2 * 1024 * 1024 * 1024, // 2GB
		Vcpus:       1,
		Cmd:         []string{"/nonexistent-command"},
	})
	require.NoError(t, err)
	t.Logf("Instance created: %s", inst.Id)

	// Wait for VM to reach running state first
	err = waitForVMReady(ctx, inst.SocketPath, 10*time.Second)
	require.NoError(t, err, "VM should reach running state")

	// Wait for the VM to stop on its own (/nonexistent-command exits 127 immediately).
	// Poll GetInstance until state becomes Stopped (init writes sentinel then reboots).
	t.Log("Waiting for VM to stop on its own (expecting exit 127)...")
	var finalInst *Instance
	for i := 0; i < 60; i++ { // up to 60 seconds
		got, err := manager.GetInstance(ctx, inst.Id)
		if err == nil && got.State == StateStopped {
			finalInst = got
			break
		}
		time.Sleep(1 * time.Second)
	}
	require.NotNil(t, finalInst, "Instance should reach Stopped state within 60 seconds")
	assert.Equal(t, StateStopped, finalInst.State)

	// Verify exit info was propagated from the serial console sentinel
	require.NotNil(t, finalInst.ExitCode, "ExitCode should be populated after app exits")
	assert.Equal(t, 127, *finalInst.ExitCode, "Non-existent command should exit with code 127")
	assert.Contains(t, finalInst.ExitMessage, "command not found", "Exit message should say command not found")
	t.Logf("Exit info propagated: code=%d message=%q", *finalInst.ExitCode, finalInst.ExitMessage)

	// Cleanup
	err = manager.DeleteInstance(ctx, inst.Id)
	require.NoError(t, err)

	t.Log("App exit propagation test complete!")
}

// TestOOMExitPropagation verifies that OOM kills are detected and reported.
// Creates a VM with low memory and runs a command that allocates more than available,
// triggering the OOM killer. Verifies exit code 137 and "OOM" in the exit message.
func TestOOMExitPropagation(t *testing.T) {
	if _, err := os.Stat("/dev/kvm"); os.IsNotExist(err) {
		t.Skip("/dev/kvm not available, skipping on this platform")
	}

	manager, tmpDir := setupTestManager(t)
	ctx := context.Background()
	p := paths.New(tmpDir)

	imageManager, err := images.NewManager(p, 1, nil)
	require.NoError(t, err)

	t.Log("Pulling alpine:latest image...")
	alpineImage, err := imageManager.CreateImage(ctx, images.CreateImageRequest{
		Name: "docker.io/library/alpine:latest",
	})
	require.NoError(t, err)

	imageName := alpineImage.Name
	for i := 0; i < 60; i++ {
		img, err := imageManager.GetImage(ctx, imageName)
		if err == nil && img.Status == images.StatusReady {
			alpineImage = img
			break
		}
		if err == nil && img.Status == images.StatusFailed {
			t.Fatalf("Image build failed: %s", *img.Error)
		}
		time.Sleep(1 * time.Second)
	}
	require.Equal(t, images.StatusReady, alpineImage.Status)
	t.Log("Alpine image ready")

	systemManager := system.NewManager(p)
	err = systemManager.EnsureSystemFiles(ctx)
	require.NoError(t, err)

	// Create instance with minimal memory (256MB) and a command that allocates
	// anonymous memory until the OOM killer fires and kills the process with SIGKILL.
	// We use a shell script that creates a large string variable in a loop, forcing
	// the shell process to grow its RSS until OOM kills it.
	inst, err := manager.CreateInstance(ctx, CreateInstanceRequest{
		Name:        "test-oom",
		Image:       "docker.io/library/alpine:latest",
		Size:        128 * 1024 * 1024, // 128MB -- small enough for OOM
		HotplugSize: 0,
		OverlaySize: 2 * 1024 * 1024 * 1024, // 2GB
		Vcpus:       1,
		Cmd:         []string{"sh", "-c", "a=x; while true; do a=$a$a$a$a; done"},
	})
	require.NoError(t, err)
	t.Logf("Instance created: %s (128MB RAM, will OOM)", inst.Id)

	err = waitForVMReady(ctx, inst.SocketPath, 10*time.Second)
	require.NoError(t, err, "VM should reach running state")

	// Wait for the VM to stop (OOM kill -> init detects -> sentinel -> reboot)
	t.Log("Waiting for VM to stop after OOM...")
	var finalInst *Instance
	for i := 0; i < 90; i++ { // up to 90 seconds (OOM may take time with low memory)
		got, err := manager.GetInstance(ctx, inst.Id)
		if err == nil && got.State == StateStopped {
			finalInst = got
			break
		}
		time.Sleep(1 * time.Second)
	}
	require.NotNil(t, finalInst, "Instance should reach Stopped state within 90 seconds")
	assert.Equal(t, StateStopped, finalInst.State)

	// Verify exit info shows OOM
	require.NotNil(t, finalInst.ExitCode, "ExitCode should be populated after OOM")
	assert.Equal(t, 137, *finalInst.ExitCode, "OOM kill should result in exit code 137 (SIGKILL)")
	assert.Contains(t, finalInst.ExitMessage, "OOM", "Exit message should indicate OOM")
	t.Logf("OOM exit info propagated: code=%d message=%q", *finalInst.ExitCode, finalInst.ExitMessage)

	// Cleanup
	err = manager.DeleteInstance(ctx, inst.Id)
	require.NoError(t, err)

	t.Log("OOM exit propagation test complete!")
}

// TestEntrypointEnvVars verifies that environment variables are passed to the entrypoint process.
// This uses bitnami/redis which configures REDIS_PASSWORD from an env var - if auth is required,
// it proves the entrypoint received and used the env var.
func TestEntrypointEnvVars(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("Skipping test that requires root")
	}

	mgr, tmpDir := setupTestManager(t) // Automatically registers cleanup
	ctx := context.Background()

	// Get image manager
	p := paths.New(tmpDir)
	imageManager, err := images.NewManager(p, 1, nil)
	require.NoError(t, err)

	// Pull bitnami/redis image
	t.Log("Pulling bitnami/redis image...")
	redisImage, err := imageManager.CreateImage(ctx, images.CreateImageRequest{
		Name: "docker.io/bitnami/redis:latest",
	})
	require.NoError(t, err)

	// Wait for image to be ready
	t.Log("Waiting for image build to complete...")
	imageName := redisImage.Name
	for i := 0; i < 60; i++ {
		img, err := imageManager.GetImage(ctx, imageName)
		if err == nil && img.Status == images.StatusReady {
			redisImage = img
			break
		}
		if err == nil && img.Status == images.StatusFailed {
			t.Fatalf("Image build failed: %s", *img.Error)
		}
		time.Sleep(1 * time.Second)
	}
	require.Equal(t, images.StatusReady, redisImage.Status, "Image should be ready after 60 seconds")
	t.Log("Redis image ready")

	// Ensure system files
	systemManager := system.NewManager(p)
	t.Log("Ensuring system files...")
	err = systemManager.EnsureSystemFiles(ctx)
	require.NoError(t, err)
	t.Log("System files ready")

	// Initialize network (needed for loopback interface in guest)
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

	// Create instance with REDIS_PASSWORD env var
	testPassword := "test_secret_password_123"
	req := CreateInstanceRequest{
		Name:           "test-redis-env",
		Image:          "docker.io/bitnami/redis:latest",
		Size:           2 * 1024 * 1024 * 1024,
		HotplugSize:    512 * 1024 * 1024,
		OverlaySize:    10 * 1024 * 1024 * 1024,
		Vcpus:          2,
		NetworkEnabled: true, // Need network for loopback to work properly
		Env: map[string]string{
			"REDIS_PASSWORD": testPassword,
		},
	}

	t.Log("Creating redis instance with REDIS_PASSWORD...")
	inst, err := mgr.CreateInstance(ctx, req)
	require.NoError(t, err)
	require.NotNil(t, inst)
	assert.Equal(t, StateRunning, inst.State)
	t.Logf("Instance created: %s", inst.Id)

	// Wait for redis to be ready (bitnami/redis takes longer to start)
	t.Log("Waiting for redis to be ready...")
	time.Sleep(15 * time.Second)

	// Helper to run command in guest with retry
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

	// Test 1: PING without auth should fail
	t.Log("Testing redis PING without auth (should fail)...")
	output, _, err := runCmd("redis-cli", "PING")
	require.NoError(t, err)
	assert.Contains(t, output, "NOAUTH", "Redis should require authentication")

	// Test 2: PING with correct password should succeed
	t.Log("Testing redis PING with correct password (should succeed)...")
	output, exitCode, err := runCmd("redis-cli", "-a", testPassword, "PING")
	require.NoError(t, err)
	require.Equal(t, 0, exitCode)
	assert.Contains(t, output, "PONG", "Redis should respond to authenticated PING")

	// Test 3: Verify requirepass config matches our env var
	t.Log("Verifying redis requirepass config...")
	output, exitCode, err = runCmd("redis-cli", "-a", testPassword, "CONFIG", "GET", "requirepass")
	require.NoError(t, err)
	require.Equal(t, 0, exitCode)
	assert.Contains(t, output, testPassword, "Redis requirepass should match REDIS_PASSWORD env var")

	t.Log("Entrypoint environment variable test passed!")

	// Cleanup
	t.Log("Cleaning up...")
	err = mgr.DeleteInstance(ctx, inst.Id)
	require.NoError(t, err)
}

func TestStorageOperations(t *testing.T) {
	// Test storage layer without starting VMs
	tmpDir := t.TempDir()

	cfg := &config.Config{
		DataDir:        tmpDir,
		BridgeName:     "vmbr0",
		SubnetCIDR:     "10.100.0.0/16",
		DNSServer:      "1.1.1.1",
		OversubCPU:     1.0,
		OversubMemory:  1.0,
		OversubDisk:    1.0,
		OversubNetwork: 1.0,
	}

	p := paths.New(tmpDir)
	imageManager, _ := images.NewManager(p, 1, nil)
	systemManager := system.NewManager(p)
	networkManager := network.NewManager(p, cfg, nil)
	deviceManager := devices.NewManager(p)
	volumeManager := volumes.NewManager(p, 0, nil) // 0 = unlimited storage
	limits := ResourceLimits{
		MaxOverlaySize:       100 * 1024 * 1024 * 1024, // 100GB
		MaxVcpusPerInstance:  0,                        // unlimited
		MaxMemoryPerInstance: 0,                        // unlimited
	}
	manager := NewManager(p, imageManager, systemManager, networkManager, deviceManager, volumeManager, limits, "", nil, nil).(*manager)

	// Test metadata doesn't exist initially
	_, err := manager.loadMetadata("nonexistent")
	assert.ErrorIs(t, err, ErrNotFound)

	// Create instance metadata (stored fields only)
	stored := &StoredMetadata{
		Id:                "test-123",
		Name:              "test",
		Image:             "test:latest",
		Size:              1024 * 1024 * 1024,
		HotplugSize:       2048 * 1024 * 1024,
		OverlaySize:       10 * 1024 * 1024 * 1024,
		Vcpus:             2,
		Env:               map[string]string{"TEST": "value"},
		CreatedAt:         time.Now(),
		HypervisorType:    hypervisor.TypeCloudHypervisor,
		HypervisorVersion: string(vmm.V49_0),
		SocketPath:        "/tmp/test.sock",
		DataDir:           paths.New(tmpDir).InstanceDir("test-123"),
	}

	// Ensure directories
	err = manager.ensureDirectories(stored.Id)
	require.NoError(t, err)

	// Save metadata
	meta := &metadata{StoredMetadata: *stored}
	err = manager.saveMetadata(meta)
	require.NoError(t, err)

	// Load metadata
	loaded, err := manager.loadMetadata(stored.Id)
	require.NoError(t, err)
	assert.Equal(t, stored.Id, loaded.Id)
	assert.Equal(t, stored.Name, loaded.Name)
	// State is no longer stored, it's derived

	// List metadata files
	files, err := manager.listMetadataFiles()
	require.NoError(t, err)
	assert.Len(t, files, 1)

	// Delete instance data
	err = manager.deleteInstanceData(stored.Id)
	require.NoError(t, err)

	// Verify deletion
	_, err = manager.loadMetadata(stored.Id)
	assert.ErrorIs(t, err, ErrNotFound)
}

func TestStandbyAndRestore(t *testing.T) {
	// Require KVM access (don't skip, fail informatively)
	if _, err := os.Stat("/dev/kvm"); os.IsNotExist(err) {
		t.Skip("/dev/kvm not available, skipping on this platform")
	}

	manager, tmpDir := setupTestManager(t) // Automatically registers cleanup
	ctx := context.Background()

	// Create image manager for pulling nginx
	imageManager, err := images.NewManager(paths.New(tmpDir), 1, nil)
	require.NoError(t, err)

	// Pull nginx image (reuse if already pulled in previous test)
	t.Log("Ensuring nginx:alpine image...")
	nginxImage, err := imageManager.CreateImage(ctx, images.CreateImageRequest{
		Name: "docker.io/library/nginx:alpine",
	})
	require.NoError(t, err)

	// Wait for image to be ready
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

	// Ensure system files
	systemManager := system.NewManager(paths.New(tmpDir))
	err = systemManager.EnsureSystemFiles(ctx)
	require.NoError(t, err)

	// Create instance
	t.Log("Creating instance...")
	req := CreateInstanceRequest{
		Name:           "test-standby",
		Image:          "docker.io/library/nginx:alpine",
		Size:           2 * 1024 * 1024 * 1024, // 2GB (needs extra room for initrd with NVIDIA libs)
		HotplugSize:    512 * 1024 * 1024,
		OverlaySize:    10 * 1024 * 1024 * 1024,
		Vcpus:          1,
		NetworkEnabled: false, // No network for tests
		Env:            map[string]string{},
	}

	inst, err := manager.CreateInstance(ctx, req)
	require.NoError(t, err)
	assert.Equal(t, StateRunning, inst.State)
	t.Logf("Instance created: %s", inst.Id)

	// Wait for VM to be fully running before standby
	err = waitForVMReady(ctx, inst.SocketPath, 5*time.Second)
	require.NoError(t, err, "VM should reach running state")

	// Standby instance
	t.Log("Standing by instance...")
	inst, err = manager.StandbyInstance(ctx, inst.Id)
	require.NoError(t, err)
	assert.Equal(t, StateStandby, inst.State)
	assert.True(t, inst.HasSnapshot)
	t.Log("Instance in standby")

	// Verify snapshot exists
	p := paths.New(tmpDir)
	snapshotDir := p.InstanceSnapshotLatest(inst.Id)
	assert.DirExists(t, snapshotDir)
	assert.FileExists(t, filepath.Join(snapshotDir, "memory-ranges"))
	// Cloud Hypervisor creates various snapshot files, just verify directory exists

	// DEBUG: Check snapshot files (for comparison with networking test)
	t.Log("DEBUG: Snapshot files for non-network instance:")
	entries, _ := os.ReadDir(snapshotDir)
	for _, entry := range entries {
		info, _ := entry.Info()
		t.Logf("  - %s (size: %d bytes)", entry.Name(), info.Size())
	}

	// DEBUG: Check app.log file size before restore
	consoleLogPath := filepath.Join(tmpDir, "guests", inst.Id, "logs", "app.log")
	var consoleLogSizeBefore int64
	if info, err := os.Stat(consoleLogPath); err == nil {
		consoleLogSizeBefore = info.Size()
		t.Logf("DEBUG: app.log size before restore: %d bytes", consoleLogSizeBefore)
	}

	// Restore instance
	t.Log("Restoring instance...")
	inst, err = manager.RestoreInstance(ctx, inst.Id)
	require.NoError(t, err)
	assert.Equal(t, StateRunning, inst.State)
	t.Log("Instance restored and running")

	// DEBUG: Check app.log file size after restore
	if info, err := os.Stat(consoleLogPath); err == nil {
		consoleLogSizeAfter := info.Size()
		t.Logf("DEBUG: app.log size after restore: %d bytes", consoleLogSizeAfter)
		t.Logf("DEBUG: File size diff: %d bytes", consoleLogSizeAfter-consoleLogSizeBefore)
		if consoleLogSizeAfter < consoleLogSizeBefore {
			t.Logf("DEBUG: WARNING! app.log was TRUNCATED (lost %d bytes)", consoleLogSizeBefore-consoleLogSizeAfter)
		}
	}

	// Cleanup (no sleep needed - DeleteInstance handles process cleanup)
	t.Log("Cleaning up...")
	err = manager.DeleteInstance(ctx, inst.Id)
	require.NoError(t, err)

	t.Log("Standby/restore test complete!")
}

func TestStateTransitions(t *testing.T) {
	tests := []struct {
		name       string
		from       State
		to         State
		shouldFail bool
	}{
		{"Stopped to Created", StateStopped, StateCreated, false},
		{"Created to Running", StateCreated, StateRunning, false},
		{"Running to Paused", StateRunning, StatePaused, false},
		{"Paused to Running", StatePaused, StateRunning, false},
		{"Paused to Standby", StatePaused, StateStandby, false},
		{"Standby to Paused", StateStandby, StatePaused, false},
		{"Shutdown to Stopped", StateShutdown, StateStopped, false},
		{"Standby to Stopped", StateStandby, StateStopped, false},
		// Invalid transitions
		{"Running to Standby", StateRunning, StateStandby, true},
		{"Stopped to Running", StateStopped, StateRunning, true},
		{"Standby to Running", StateStandby, StateRunning, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.from.CanTransitionTo(tt.to)
			if tt.shouldFail {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

// No mock image manager needed - tests use real images!

// testInstanceResolver is a simple implementation of ingress.InstanceResolver for testing.
type testInstanceResolver struct {
	ip     string
	exists bool
}

func (r *testInstanceResolver) ResolveInstanceIP(ctx context.Context, nameOrID string) (string, error) {
	if r.ip == "" {
		return "", fmt.Errorf("instance not found: %s", nameOrID)
	}
	return r.ip, nil
}

func (r *testInstanceResolver) InstanceExists(ctx context.Context, nameOrID string) (bool, error) {
	return r.exists, nil
}

func (r *testInstanceResolver) ResolveInstance(ctx context.Context, nameOrID string) (string, string, error) {
	if !r.exists {
		return "", "", fmt.Errorf("instance not found: %s", nameOrID)
	}
	// For tests, just return nameOrID as both name and id
	return nameOrID, nameOrID, nil
}
