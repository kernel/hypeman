package api

import (
	"bytes"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/onkernel/hypeman/lib/exec"
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
	timeout := time.After(30 * time.Second)
	ticker := time.NewTicker(1 * time.Second)
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
	networkDisabled := false
	instResp, err := svc.CreateInstance(ctx(), oapi.CreateInstanceRequestObject{
		Body: &oapi.CreateInstanceRequest{
			Name:  "exec-test",
			Image: "docker.io/library/nginx:alpine",
			Network: &struct {
				Enabled *bool `json:"enabled,omitempty"`
			}{
				Enabled: &networkDisabled,
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
			logChan, err := svc.InstanceManager.StreamInstanceLogs(ctx(), inst.Id, 100, false)
			if err == nil {
				var logs strings.Builder
				for line := range logChan {
					logs.WriteString(line)
				}
				if strings.Contains(logs.String(), "start worker processes") {
					nginxReady = true
					t.Log("Nginx is ready")
				}
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

	// Capture console log on failure with exec-agent filtering
	t.Cleanup(func() {
		if t.Failed() {
			consolePath := paths.New(svc.Config.DataDir).InstanceConsoleLog(inst.Id)
			if consoleData, err := os.ReadFile(consolePath); err == nil {
				lines := strings.Split(string(consoleData), "\n")
				
				// Print exec-agent specific logs
				t.Logf("=== Exec Agent Logs ===")
				for _, line := range lines {
					if strings.Contains(line, "[exec-agent]") {
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

	// Wait for exec agent to be ready (retry a few times)
	var exit *exec.ExitStatus
	var stdout, stderr outputBuffer
	var execErr error
	
	t.Log("Testing exec command: whoami")
	maxRetries := 10
	for i := 0; i < maxRetries; i++ {
		stdout = outputBuffer{}
		stderr = outputBuffer{}
		
		exit, execErr = exec.ExecIntoInstance(ctx(), actualInst.VsockSocket, exec.ExecOptions{
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
