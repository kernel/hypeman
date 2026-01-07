package api

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/onkernel/hypeman/lib/guest"
	"github.com/onkernel/hypeman/lib/hypervisor"
	"github.com/onkernel/hypeman/lib/oapi"
	"github.com/onkernel/hypeman/lib/paths"
	"github.com/onkernel/hypeman/lib/system"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCpToAndFromInstance(t *testing.T) {
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

	// Create and wait for nginx image (has a long-running process)
	createAndWaitForImage(t, svc, "docker.io/library/nginx:alpine", 30*time.Second)

	// Create instance
	t.Log("Creating instance...")
	networkEnabled := false
	instResp, err := svc.CreateInstance(ctx(), oapi.CreateInstanceRequestObject{
		Body: &oapi.CreateInstanceRequest{
			Name:  "cp-test",
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

	// Wait for guest-agent to be ready
	t.Log("Waiting for guest-agent to start...")
	agentReady := false
	agentTimeout := time.After(15 * time.Second)
	agentTicker := time.NewTicker(500 * time.Millisecond)
	defer agentTicker.Stop()

	for !agentReady {
		select {
		case <-agentTimeout:
			logs := collectTestLogs(t, svc, inst.Id, 200)
			t.Logf("Console logs:\n%s", logs)
			t.Fatal("Timeout waiting for guest-agent to start")
		case <-agentTicker.C:
			logs := collectTestLogs(t, svc, inst.Id, 100)
			if strings.Contains(logs, "[guest-agent] listening on vsock port 2222") {
				agentReady = true
				t.Log("guest-agent is ready")
			}
		}
	}

	// Get actual instance to access vsock fields
	actualInst, err := svc.InstanceManager.GetInstance(ctx(), inst.Id)
	require.NoError(t, err)
	require.NotNil(t, actualInst)

	t.Logf("vsock CID: %d, socket: %s", actualInst.VsockCID, actualInst.VsockSocket)

	// Capture console log on failure
	t.Cleanup(func() {
		if t.Failed() {
			consolePath := paths.New(svc.Config.DataDir).InstanceAppLog(inst.Id)
			if consoleData, err := os.ReadFile(consolePath); err == nil {
				lines := strings.Split(string(consoleData), "\n")
				t.Logf("=== Guest Agent Logs ===")
				for _, line := range lines {
					if strings.Contains(line, "[guest-agent]") {
						t.Logf("%s", line)
					}
				}
			}
		}
	})

	// Create a temporary file to copy
	testContent := "Hello from hypeman cp test!\nLine 2\nLine 3\n"
	srcFile := filepath.Join(t.TempDir(), "test-file.txt")
	err = os.WriteFile(srcFile, []byte(testContent), 0644)
	require.NoError(t, err)

	// Create vsock dialer
	dialer, err := hypervisor.NewVsockDialer(actualInst.HypervisorType, actualInst.VsockSocket, actualInst.VsockCID)
	require.NoError(t, err)

	// Test 1: Copy file TO instance
	t.Log("Testing CopyToInstance...")
	dstPath := "/tmp/copied-file.txt"
	err = guest.CopyToInstance(ctx(), dialer, guest.CopyToInstanceOptions{
		SrcPath: srcFile,
		DstPath: dstPath,
	})
	require.NoError(t, err, "CopyToInstance should succeed")

	// Verify the file was copied by reading it back via exec
	t.Log("Verifying file was copied via exec...")
	var stdout, stderr outputBuffer
	exit, err := guest.ExecIntoInstance(ctx(), dialer, guest.ExecOptions{
		Command: []string{"cat", dstPath},
		Stdout:  &stdout,
		Stderr:  &stderr,
		TTY:     false,
	})
	require.NoError(t, err)
	require.Equal(t, 0, exit.Code, "cat should succeed")
	assert.Equal(t, testContent, stdout.String(), "file content should match")

	// Test 2: Copy file FROM instance
	t.Log("Testing CopyFromInstance...")
	localDstDir := t.TempDir()
	err = guest.CopyFromInstance(ctx(), dialer, guest.CopyFromInstanceOptions{
		SrcPath: dstPath,
		DstPath: localDstDir,
	})
	require.NoError(t, err, "CopyFromInstance should succeed")

	// Verify the file was copied back
	copiedBack, err := os.ReadFile(filepath.Join(localDstDir, "copied-file.txt"))
	require.NoError(t, err)
	assert.Equal(t, testContent, string(copiedBack), "copied back content should match")

	t.Log("Cp tests passed!")
}

func TestCpDirectoryToInstance(t *testing.T) {
	// Require KVM access for VM creation
	if _, err := os.Stat("/dev/kvm"); os.IsNotExist(err) {
		t.Fatal("/dev/kvm not available - ensure KVM is enabled and user is in 'kvm' group (sudo usermod -aG kvm $USER)")
	}

	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	svc := newTestService(t)

	// Ensure system files
	t.Log("Ensuring system files...")
	systemMgr := system.NewManager(paths.New(svc.Config.DataDir))
	err := systemMgr.EnsureSystemFiles(ctx())
	require.NoError(t, err)

	// Create and wait for nginx image (has a long-running process)
	createAndWaitForImage(t, svc, "docker.io/library/nginx:alpine", 30*time.Second)

	// Create instance
	t.Log("Creating instance...")
	networkEnabled := false
	instResp, err := svc.CreateInstance(ctx(), oapi.CreateInstanceRequestObject{
		Body: &oapi.CreateInstanceRequest{
			Name:  "cp-dir-test",
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
	t.Logf("Instance created: %s", inst.Id)

	// Wait for guest-agent
	t.Log("Waiting for guest-agent...")
	agentReady := false
	agentTimeout := time.After(15 * time.Second)
	agentTicker := time.NewTicker(500 * time.Millisecond)
	defer agentTicker.Stop()

	for !agentReady {
		select {
		case <-agentTimeout:
			t.Fatal("Timeout waiting for guest-agent")
		case <-agentTicker.C:
			logs := collectTestLogs(t, svc, inst.Id, 100)
			if strings.Contains(logs, "[guest-agent] listening on vsock port 2222") {
				agentReady = true
			}
		}
	}

	actualInst, err := svc.InstanceManager.GetInstance(ctx(), inst.Id)
	require.NoError(t, err)

	// Create vsock dialer
	dialer, err := hypervisor.NewVsockDialer(actualInst.HypervisorType, actualInst.VsockSocket, actualInst.VsockCID)
	require.NoError(t, err)

	// Create a test directory structure
	srcDir := filepath.Join(t.TempDir(), "testdir")
	require.NoError(t, os.MkdirAll(filepath.Join(srcDir, "subdir"), 0755))
	require.NoError(t, os.WriteFile(filepath.Join(srcDir, "file1.txt"), []byte("file1 content"), 0644))
	require.NoError(t, os.WriteFile(filepath.Join(srcDir, "subdir", "file2.txt"), []byte("file2 content"), 0644))

	// Copy directory to instance
	t.Log("Copying directory to instance...")
	err = guest.CopyToInstance(ctx(), dialer, guest.CopyToInstanceOptions{
		SrcPath: srcDir,
		DstPath: "/tmp/testdir",
	})
	require.NoError(t, err)

	// Verify files exist via exec
	var stdout outputBuffer
	exit, err := guest.ExecIntoInstance(ctx(), dialer, guest.ExecOptions{
		Command: []string{"cat", "/tmp/testdir/file1.txt"},
		Stdout:  &stdout,
		TTY:     false,
	})
	require.NoError(t, err)
	require.Equal(t, 0, exit.Code)
	assert.Equal(t, "file1 content", stdout.String())

	stdout = outputBuffer{}
	exit, err = guest.ExecIntoInstance(ctx(), dialer, guest.ExecOptions{
		Command: []string{"cat", "/tmp/testdir/subdir/file2.txt"},
		Stdout:  &stdout,
		TTY:     false,
	})
	require.NoError(t, err)
	require.Equal(t, 0, exit.Code)
	assert.Equal(t, "file2 content", stdout.String())

	// Copy directory from instance
	t.Log("Copying directory from instance...")
	localDstDir := t.TempDir()
	err = guest.CopyFromInstance(ctx(), dialer, guest.CopyFromInstanceOptions{
		SrcPath: "/tmp/testdir",
		DstPath: localDstDir,
	})
	require.NoError(t, err)

	// Verify files were copied back
	content1, err := os.ReadFile(filepath.Join(localDstDir, "testdir", "file1.txt"))
	require.NoError(t, err)
	assert.Equal(t, "file1 content", string(content1))

	content2, err := os.ReadFile(filepath.Join(localDstDir, "testdir", "subdir", "file2.txt"))
	require.NoError(t, err)
	assert.Equal(t, "file2 content", string(content2))

	t.Log("Directory cp tests passed!")
}
