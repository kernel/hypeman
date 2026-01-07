package api

import (
	"bytes"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/onkernel/hypeman/lib/guest"
	"github.com/onkernel/hypeman/lib/hypervisor"
	"github.com/onkernel/hypeman/lib/instances"
	"github.com/onkernel/hypeman/lib/oapi"
	"github.com/onkernel/hypeman/lib/paths"
	"github.com/onkernel/hypeman/lib/system"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestExecInstanceNonTTY(t *testing.T) {
	// Require KVM access for VM creation
	if _, err := os.Stat("/dev/kvm"); os.IsNotExist(err) {
		t.Fatal("/dev/kvm not available - ensure KVM is enabled and user is in 'kvm' group (sudo usermod -aG kvm $USER)")
	}

	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	svc := newTestService(t)

	// Ensure system files (kernel and initrd) are available
	t.Log("Ensuring system files...")
	systemMgr := system.NewManager(paths.New(svc.Config.DataDir))
	err := systemMgr.EnsureSystemFiles(ctx())
	require.NoError(t, err)
	t.Log("System files ready")

	// Create and wait for nginx image (has a proper long-running process)
	createAndWaitForImage(t, svc, "docker.io/library/nginx:alpine", 30*time.Second)

	// Create instance
	t.Log("Creating instance...")
	networkEnabled := false
	instResp, err := svc.CreateInstance(ctx(), oapi.CreateInstanceRequestObject{
		Body: &oapi.CreateInstanceRequest{
			Name:  "exec-test",
			Image: "docker.io/library/nginx:alpine",
			Network: &struct {
				BandwidthDownload *string `json:"bandwidth_download,omitempty"`
				BandwidthUpload   *string `json:"bandwidth_upload,omitempty"`
				Enabled           *bool   `json:"enabled,omitempty"`
			}{
				Enabled: &networkEnabled,
			},
		},
	})
	require.NoError(t, err)

	inst, ok := instResp.(oapi.CreateInstance201JSONResponse)
	require.True(t, ok, "expected 201 response")
	require.NotEmpty(t, inst.Id)
	t.Logf("Instance created: %s", inst.Id)

	// Wait for nginx to be fully started (poll console logs)
	t.Log("Waiting for nginx to start...")
	nginxReady := false
	nginxTimeout := time.After(15 * time.Second)
	nginxTicker := time.NewTicker(500 * time.Millisecond)
	defer nginxTicker.Stop()

	for !nginxReady {
		select {
		case <-nginxTimeout:
			t.Fatal("Timeout waiting for nginx to start")
		case <-nginxTicker.C:
			logs := collectTestLogs(t, svc, inst.Id, 100)
			if strings.Contains(logs, "start worker processes") {
				nginxReady = true
				t.Log("Nginx is ready")
			}
		}
	}

	// Get actual instance to access vsock fields
	actualInst, err := svc.InstanceManager.GetInstance(ctx(), inst.Id)
	require.NoError(t, err)
	require.NotNil(t, actualInst)

	// Verify vsock fields are set
	require.Greater(t, actualInst.VsockCID, int64(2), "vsock CID should be > 2 (reserved values)")
	require.NotEmpty(t, actualInst.VsockSocket, "vsock socket path should be set")
	t.Logf("vsock CID: %d, socket: %s", actualInst.VsockCID, actualInst.VsockSocket)

	// Capture console log on failure with guest-agent filtering
	t.Cleanup(func() {
		if t.Failed() {
			consolePath := paths.New(svc.Config.DataDir).InstanceAppLog(inst.Id)
			if consoleData, err := os.ReadFile(consolePath); err == nil {
				lines := strings.Split(string(consoleData), "\n")

				// Print guest-agent specific logs
				t.Logf("=== Guest Agent Logs ===")
				for _, line := range lines {
					if strings.Contains(line, "[guest-agent]") {
						t.Logf("%s", line)
					}
				}
			}
		}
	})

	// Check if vsock socket exists
	if _, err := os.Stat(actualInst.VsockSocket); err != nil {
		t.Logf("vsock socket does not exist: %v", err)
	} else {
		t.Logf("vsock socket exists: %s", actualInst.VsockSocket)
	}

	var stdout, stderr outputBuffer

	dialer, err := hypervisor.NewVsockDialer(actualInst.HypervisorType, actualInst.VsockSocket, actualInst.VsockCID)
	require.NoError(t, err)

	t.Log("Testing exec command: whoami")
	exit, execErr := guest.ExecIntoInstance(ctx(), dialer, guest.ExecOptions{
		Command:      []string{"/bin/sh", "-c", "whoami"},
		Stdin:        nil,
		Stdout:       &stdout,
		Stderr:       &stderr,
		TTY:          false,
		WaitForAgent: 10 * time.Second, // Wait up to 10s for guest agent to be ready
	})

	// Assert exec worked
	require.NoError(t, execErr, "exec should succeed")
	require.NotNil(t, exit, "exit status should be returned")
	require.Equal(t, 0, exit.Code, "whoami should exit with code 0")

	// Verify output
	outStr := stdout.String()
	t.Logf("Command output: %q", outStr)
	require.Contains(t, outStr, "root", "whoami should return root user")

	// Cleanup
	t.Log("Cleaning up instance...")
	delResp, err := svc.DeleteInstance(ctxWithInstance(svc, inst.Id), oapi.DeleteInstanceRequestObject{
		Id: inst.Id,
	})
	require.NoError(t, err)
	_, ok = delResp.(oapi.DeleteInstance204Response)
	require.True(t, ok, "expected 204 response")
}

// TestExecWithDebianMinimal tests exec with a minimal Debian image.
// This test specifically catches issues that wouldn't appear with Alpine-based images:
// 1. Debian's default entrypoint (bash) exits immediately without a TTY
// 2. guest-agent must keep running even after the main app exits
// 3. The VM must not kernel panic when the entrypoint exits
func TestExecWithDebianMinimal(t *testing.T) {
	// Require KVM access for VM creation
	if _, err := os.Stat("/dev/kvm"); os.IsNotExist(err) {
		t.Fatal("/dev/kvm not available - ensure KVM is enabled and user is in 'kvm' group (sudo usermod -aG kvm $USER)")
	}

	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	svc := newTestService(t)

	// Ensure system files (kernel and initrd) are available
	t.Log("Ensuring system files...")
	systemMgr := system.NewManager(paths.New(svc.Config.DataDir))
	err := systemMgr.EnsureSystemFiles(ctx())
	require.NoError(t, err)
	t.Log("System files ready")

	// Create Debian 12 slim image (minimal, no iproute2)
	createAndWaitForImage(t, svc, "docker.io/library/debian:12-slim", 60*time.Second)

	// Create instance (network disabled in test environment)
	t.Log("Creating Debian instance...")
	networkEnabled := false
	instResp, err := svc.CreateInstance(ctx(), oapi.CreateInstanceRequestObject{
		Body: &oapi.CreateInstanceRequest{
			Name:  "debian-exec-test",
			Image: "docker.io/library/debian:12-slim",
			Network: &struct {
				BandwidthDownload *string `json:"bandwidth_download,omitempty"`
				BandwidthUpload   *string `json:"bandwidth_upload,omitempty"`
				Enabled           *bool   `json:"enabled,omitempty"`
			}{
				Enabled: &networkEnabled,
			},
		},
	})
	require.NoError(t, err)

	inst, ok := instResp.(oapi.CreateInstance201JSONResponse)
	require.True(t, ok, "expected 201 response")
	require.NotEmpty(t, inst.Id)
	t.Logf("Instance created: %s", inst.Id)

	// Cleanup on exit
	t.Cleanup(func() {
		t.Log("Cleaning up instance...")
		svc.DeleteInstance(ctxWithInstance(svc, inst.Id), oapi.DeleteInstanceRequestObject{Id: inst.Id})
	})

	// Get actual instance to access vsock fields
	actualInst, err := svc.InstanceManager.GetInstance(ctx(), inst.Id)
	require.NoError(t, err)
	require.NotNil(t, actualInst)

	// Wait for guest-agent to be ready by checking logs
	// This is the key difference: we wait for guest-agent, not the app (which exits immediately)
	t.Log("Waiting for guest-agent to start...")
	execAgentReady := false
	agentTimeout := time.After(15 * time.Second)
	agentTicker := time.NewTicker(500 * time.Millisecond)
	defer agentTicker.Stop()

	var logs string
	for !execAgentReady {
		select {
		case <-agentTimeout:
			// Dump logs on failure for debugging
			logs = collectTestLogs(t, svc, inst.Id, 200)
			t.Logf("Console logs:\n%s", logs)
			t.Fatal("Timeout waiting for guest-agent to start")
		case <-agentTicker.C:
			logs = collectTestLogs(t, svc, inst.Id, 100)
			if strings.Contains(logs, "[guest-agent] listening on vsock port 2222") {
				execAgentReady = true
				t.Log("guest-agent is ready")
			}
		}
	}

	// Verify the app exited but VM is still usable (key behavior this test validates)
	logs = collectTestLogs(t, svc, inst.Id, 200)
	assert.Contains(t, logs, "[exec] app exited with code", "App should have exited")

	// Test exec commands work even though the main app (bash) has exited
	dialer2, err := hypervisor.NewVsockDialer(actualInst.HypervisorType, actualInst.VsockSocket, actualInst.VsockCID)
	require.NoError(t, err)

	t.Log("Testing exec command: echo")
	var stdout, stderr outputBuffer
	exit, err := guest.ExecIntoInstance(ctx(), dialer2, guest.ExecOptions{
		Command: []string{"echo", "hello from debian"},
		Stdout:  &stdout,
		Stderr:  &stderr,
		TTY:     false,
	})
	require.NoError(t, err, "exec should succeed")
	require.NotNil(t, exit)
	require.Equal(t, 0, exit.Code, "echo should exit with code 0")
	assert.Contains(t, stdout.String(), "hello from debian")

	// Verify we're actually in Debian
	t.Log("Verifying OS release...")
	stdout = outputBuffer{}
	exit, err = guest.ExecIntoInstance(ctx(), dialer2, guest.ExecOptions{
		Command: []string{"cat", "/etc/os-release"},
		Stdout:  &stdout,
		TTY:     false,
	})
	require.NoError(t, err)
	require.Equal(t, 0, exit.Code)
	assert.Contains(t, stdout.String(), "Debian", "Should be running Debian")
	assert.Contains(t, stdout.String(), "bookworm", "Should be Debian 12 (bookworm)")
	t.Logf("OS: %s", strings.Split(stdout.String(), "\n")[0])

}

// collectTestLogs collects logs from an instance (non-streaming)
func collectTestLogs(t *testing.T, svc *ApiService, instanceID string, n int) string {
	logChan, err := svc.InstanceManager.StreamInstanceLogs(ctx(), instanceID, n, false, instances.LogSourceApp)
	if err != nil {
		return ""
	}

	var lines []string
	for line := range logChan {
		lines = append(lines, line)
	}

	return strings.Join(lines, "\n")
}

// outputBuffer is a simple buffer for capturing exec output
type outputBuffer struct {
	buf bytes.Buffer
}

func (b *outputBuffer) Write(p []byte) (n int, err error) {
	return b.buf.Write(p)
}

func (b *outputBuffer) String() string {
	return b.buf.String()
}
