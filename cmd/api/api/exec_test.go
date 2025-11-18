package api

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

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

	// First, create and wait for the image to be ready
	// Use nginx which has a proper long-running process
	t.Log("Creating nginx:alpine image...")
	imgResp, err := svc.CreateImage(ctx(), oapi.CreateImageRequestObject{
		Body: &oapi.CreateImageRequest{
			Name: "docker.io/library/nginx:alpine",
		},
	})
	require.NoError(t, err)
	imgCreated, ok := imgResp.(oapi.CreateImage202JSONResponse)
	require.True(t, ok, "expected 202 response")
	assert.Equal(t, "docker.io/library/nginx:alpine", imgCreated.Name)

	// Wait for image to be ready (poll with timeout)
	t.Log("Waiting for image to be ready...")
	timeout := time.After(120 * time.Second)
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	imageReady := false
	for !imageReady {
		select {
		case <-timeout:
			t.Fatal("Timeout waiting for image to be ready")
		case <-ticker.C:
			imgResp, err := svc.GetImage(ctx(), oapi.GetImageRequestObject{
				Name: "docker.io/library/nginx:alpine",
			})
			require.NoError(t, err)
			
			img, ok := imgResp.(oapi.GetImage200JSONResponse)
			if ok && img.Status == "ready" {
				imageReady = true
				t.Log("Image is ready")
			} else if ok {
				t.Logf("Image status: %s", img.Status)
			}
		}
	}

	// Create instance
	t.Log("Creating instance...")
	instResp, err := svc.CreateInstance(ctx(), oapi.CreateInstanceRequestObject{
		Body: &oapi.CreateInstanceRequest{
			Name:  "exec-test",
			Image: "docker.io/library/nginx:alpine",
		},
	})
	require.NoError(t, err)

	inst, ok := instResp.(oapi.CreateInstance201JSONResponse)
	require.True(t, ok, "expected 201 response")
	require.NotEmpty(t, inst.Id)
	t.Logf("Instance created: %s", inst.Id)

	// Wait a bit for instance to fully boot
	time.Sleep(5 * time.Second)

	// Get actual instance to access vsock fields
	actualInst, err := svc.InstanceManager.GetInstance(ctx(), inst.Id)
	require.NoError(t, err)
	require.NotNil(t, actualInst)

	// Verify vsock fields are set
	require.Greater(t, actualInst.VsockCID, int64(2), "vsock CID should be > 2 (reserved values)")
	require.NotEmpty(t, actualInst.VsockSocket, "vsock socket path should be set")
	t.Logf("vsock CID: %d, socket: %s", actualInst.VsockCID, actualInst.VsockSocket)

	// Print console log for debugging
	consolePath := paths.New(svc.Config.DataDir).InstanceConsoleLog(inst.Id)
	if consoleData, err := os.ReadFile(consolePath); err == nil {
		t.Logf("=== Console Log ===")
		lines := bytes.Split(consoleData, []byte("\n"))
		
		// Print boot messages with virtio
		t.Logf("--- Boot messages (virtio devices) ---")
		for _, line := range lines {
			lineStr := string(line)
			if bytes.Contains(line, []byte("virtio")) || bytes.Contains(line, []byte("vsock")) {
				t.Logf("%s", lineStr)
			}
		}
		
		// Print last 30 lines
		t.Logf("--- Last 30 lines ---")
		start := len(lines) - 30
		if start < 0 {
			start = 0
		}
		for _, line := range lines[start:] {
			if len(line) > 0 {
				t.Logf("%s", line)
			}
		}
		t.Logf("=== End Console Log ===")
	} else {
		t.Logf("Could not read console log: %v", err)
	}
	
	// Check if vsock socket exists
	if _, err := os.Stat(actualInst.VsockSocket); err != nil {
		t.Logf("vsock socket does not exist: %v", err)
	} else {
		t.Logf("vsock socket exists: %s", actualInst.VsockSocket)
	}
	
	// Check if exec-agent exists in the initrd
	initrdPath, _ := systemMgr.GetInitrdPath(system.InitrdV2_0_0)
	t.Logf("Initrd path: %s", initrdPath)
	if _, err := os.Stat(initrdPath); err != nil {
		t.Logf("Initrd file does not exist: %v", err)
	} else {
		if stat, err := os.Stat(initrdPath); err == nil {
			t.Logf("Initrd file exists, size: %d bytes", stat.Size())
			
			// Unpack initrd to check contents
			tmpUnpack := filepath.Join(os.TempDir(), "initrd-check")
			os.RemoveAll(tmpUnpack)
			os.MkdirAll(tmpUnpack, 0755)
			
			cmd := exec.Command("sh", "-c", fmt.Sprintf("cd %s && cpio -i < %s 2>/dev/null", tmpUnpack, initrdPath))
			if err := cmd.Run(); err == nil {
				if _, err := os.Stat(filepath.Join(tmpUnpack, "usr/local/bin/exec-agent")); err == nil {
					t.Logf("✅ exec-agent found in initrd")
				} else {
					t.Logf("❌ exec-agent NOT found in initrd!")
					// List what's actually in the initrd
					entries, _ := os.ReadDir(filepath.Join(tmpUnpack, "usr/local/bin"))
					t.Logf("Contents of /usr/local/bin in initrd: %v", len(entries))
				}
			}
			os.RemoveAll(tmpUnpack)
		}
	}

	// Wait for exec agent to be ready (retry a few times)
	var exit *system.ExitStatus
	var stdout, stderr outputBuffer
	var execErr error
	
	t.Log("Testing exec command: whoami")
	maxRetries := 10
	for i := 0; i < maxRetries; i++ {
		stdout = outputBuffer{}
		stderr = outputBuffer{}
		
		exit, execErr = system.ExecIntoInstance(ctx(), actualInst.VsockSocket, system.ExecOptions{
			Command: []string{"/bin/sh", "-c", "whoami"},
			Stdin:   nil,
			Stdout:  &stdout,
			Stderr:  &stderr,
			TTY:     false,
		})
		
		if execErr == nil {
			break
		}
		
		t.Logf("Exec attempt %d/%d failed, retrying: %v", i+1, maxRetries, execErr)
		time.Sleep(1 * time.Second)
	}
	
	// Assert exec worked
	require.NoError(t, execErr, "exec should succeed after retries")
	require.NotNil(t, exit, "exit status should be returned")
	require.Equal(t, 0, exit.Code, "whoami should exit with code 0")
	
	// Verify output
	outStr := stdout.String()
	t.Logf("Command output: %q", outStr)
	require.Contains(t, outStr, "root", "whoami should return root user")
	
	// Test another command to verify filesystem access
	t.Log("Testing exec command: ls /")
	stdout = outputBuffer{}
	stderr = outputBuffer{}
	
	exit, err = system.ExecIntoInstance(ctx(), actualInst.VsockSocket, system.ExecOptions{
		Command: []string{"/bin/sh", "-c", "ls -la /"},
		Stdin:   nil,
		Stdout:  &stdout,
		Stderr:  &stderr,
		TTY:     false,
	})
	
	require.NoError(t, err, "ls command should succeed")
	require.Equal(t, 0, exit.Code, "ls should exit with code 0")
	
	outStr = stdout.String()
	t.Logf("ls output: %q", outStr)
	require.Contains(t, outStr, "bin", "should see bin directory")

	// Cleanup
	t.Log("Cleaning up instance...")
	delResp, err := svc.DeleteInstance(ctx(), oapi.DeleteInstanceRequestObject{
		Id: inst.Id,
	})
	require.NoError(t, err)
	_, ok = delResp.(oapi.DeleteInstance204Response)
	require.True(t, ok, "expected 204 response")
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

// TestVsockCIDGeneration tests the vsock CID generation logic
func TestVsockCIDGeneration(t *testing.T) {
	testCases := []struct {
		id          string
		expectedMin int64
		expectedMax int64
	}{
		{"abc123", 3, 4294967294},
		{"xyz789", 3, 4294967294},
		{"test-id-here", 3, 4294967294},
		{"a", 3, 4294967294},
		{"verylonginstanceid12345", 3, 4294967294},
	}

	for _, tc := range testCases {
		t.Run(tc.id, func(t *testing.T) {
			cid := generateVsockCID(tc.id)
			require.GreaterOrEqual(t, cid, tc.expectedMin, "CID must be >= 3")
			require.LessOrEqual(t, cid, tc.expectedMax, "CID must be < 2^32-1")
		})
	}

	// Test consistency - same ID should always produce same CID
	cid1 := generateVsockCID("consistent-test")
	cid2 := generateVsockCID("consistent-test")
	require.Equal(t, cid1, cid2, "Same instance ID should produce same CID")
}

// generateVsockCID is re-declared here for testing
func generateVsockCID(instanceID string) int64 {
	idPrefix := instanceID
	if len(idPrefix) > 8 {
		idPrefix = idPrefix[:8]
	}

	var sum int64
	for _, c := range idPrefix {
		sum = sum*37 + int64(c)
	}

	return (sum % 4294967292) + 3
}

