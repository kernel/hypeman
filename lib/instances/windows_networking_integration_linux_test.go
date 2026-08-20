//go:build linux && amd64

package instances

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kernel/hypeman/lib/forkvm"
	"github.com/kernel/hypeman/lib/guest"
	"github.com/kernel/hypeman/lib/hypervisor"
	"github.com/kernel/hypeman/lib/images"
	"github.com/kernel/hypeman/lib/paths"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWindowsLifecycleIntegration(t *testing.T) {
	if os.Getenv("HYPEMAN_RUN_WINDOWS_LIFECYCLE_INTEGRATION") != "1" {
		t.Skip("run by the dedicated Windows networking CI gate")
	}
	fixture := os.Getenv("HYPEMAN_WINDOWS_TEST_AGENT_IMAGE")
	if fixture == "" {
		fixture = "/ci/windows/image-agent.qcow2"
	}
	if _, err := os.Stat(fixture); err != nil {
		if os.Getenv("CI") == "true" {
			t.Fatalf("required Windows networking fixture is missing: %s", fixture)
		}
		t.Skipf("Windows networking fixture is unavailable: %s", fixture)
	}
	acquireHeavyIO(t)

	manager, dataDir := setupTestManagerForQEMU(t)
	p := paths.New(dataDir)
	const digestHex = "abababababababababababababababababababababababababababababababab"
	image := &images.Image{
		Name:     "registry.example/windows/image:networking-integration",
		Digest:   "sha256:" + digestHex,
		Platform: "windows/amd64",
		Status:   images.StatusReady,
		Machine: &images.MachineImage{
			Kind:        images.MachineImageWindowsImage,
			Base:        "registry.example/windows/base@sha256:cdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcd",
			TPM:         "2.0",
			SecureBoot:  "required",
			VirtualSize: 80 << 30,
		},
	}
	manager.imageManager = windowsFixtureImageManager{image: image}
	imagePath, err := images.GetMachineDiskPath(p, image.Name, image.Digest, image.Machine)
	require.NoError(t, err)
	require.NoError(t, forkvm.CopyRegularFile(fixture, imagePath))
	require.NoError(t, os.Chmod(imagePath, 0444))

	ctx := context.Background()
	instance, err := manager.CreateInstance(ctx, CreateInstanceRequest{
		Name:           "windows-networking-integration",
		Image:          image.Name,
		Platform:       "windows/amd64",
		Size:           8 << 30,
		Vcpus:          4,
		NetworkEnabled: true,
		Hypervisor:     hypervisor.TypeQEMU,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = deleteTestInstanceNow(context.Background(), manager, instance.Id) })
	require.NotEmpty(t, instance.IP)
	require.NotEmpty(t, instance.MAC)
	require.Eventually(t, func() bool {
		current, err := manager.GetInstance(ctx, instance.Id)
		return err == nil && current.State == StateRunning
	}, 4*time.Minute, time.Second)

	assertWindowsGuestControl(t, ctx, manager, instance.Id)
	assertWindowsNetworkReady(t, ctx, manager, instance.Id, instance.IP)
}

func assertWindowsGuestControl(t *testing.T, ctx context.Context, manager *manager, instanceID string) {
	t.Helper()
	dialer, err := manager.GetVsockDialer(ctx, instanceID)
	require.NoError(t, err)

	var stdout, stderr bytes.Buffer
	jobStart := time.Now()
	exit, err := guest.ExecIntoInstance(ctx, dialer, guest.ExecOptions{
		Command: []string{"powershell.exe", "-NoProfile", "-NonInteractive", "-Command", `Copy-Item "$env:SystemRoot\System32\ping.exe" "$env:TEMP\hypeman-job-child.exe" -Force; & "$env:TEMP\hypeman-job-child.exe" -n 60 127.0.0.1`},
		Stdout:  &stdout,
		Stderr:  &stderr,
		Timeout: 2,
	})
	require.NoError(t, err, stderr.String())
	assert.Less(t, time.Since(jobStart), 10*time.Second)

	time.Sleep(5 * time.Second)
	stdout.Reset()
	stderr.Reset()
	exit, err = guest.ExecIntoInstance(ctx, dialer, guest.ExecOptions{
		Command: []string{"powershell.exe", "-NoProfile", "-NonInteractive", "-Command", `if (Get-Process hypeman-job-child -ErrorAction SilentlyContinue) { exit 42 }`},
		Stdout:  &stdout,
		Stderr:  &stderr,
		Timeout: 15,
	})
	require.NoError(t, err, stderr.String())
	require.Equal(t, 0, exit.Code, "job object left a child process running")

	stdout.Reset()
	stderr.Reset()
	exit, err = guest.ExecIntoInstance(ctx, dialer, guest.ExecOptions{
		Command: []string{"powershell.exe", "-NoProfile", "-NonInteractive", "-Command", "[Console]::Out.Write('HYPEMAN_SYSTEM_OK')"},
		Stdout:  &stdout,
		Stderr:  &stderr,
		Timeout: 30,
	})
	require.NoError(t, err, stderr.String())
	require.Equal(t, 0, exit.Code)
	assert.Equal(t, "HYPEMAN_SYSTEM_OK", stdout.String())

	stdout.Reset()
	stderr.Reset()
	exit, err = guest.ExecIntoInstance(ctx, dialer, guest.ExecOptions{
		Command: []string{"cmd.exe", "/d", "/c", "ping -n 2 127.0.0.1 >nul & echo HYPEMAN_CONPTY_OK"},
		Stdout:  &stdout,
		Stderr:  &stderr,
		TTY:     true,
		Rows:    31,
		Cols:    97,
		Timeout: 30,
	})
	require.NoError(t, err, stderr.String())
	require.Equal(t, 0, exit.Code)
	assert.Contains(t, stdout.String(), "HYPEMAN_CONPTY_OK")

	stdout.Reset()
	stderr.Reset()
	exit, err = guest.ExecIntoInstance(ctx, dialer, guest.ExecOptions{
		Command: []string{"cmd.exe", "/d", "/c", "echo HYPEMAN_DESKTOP_OK"},
		Stdout:  &stdout,
		Stderr:  &stderr,
		Session: guest.ExecSession_EXEC_SESSION_DESKTOP,
		Timeout: 30,
	})
	require.NoError(t, err, stderr.String())
	require.Equal(t, 0, exit.Code)
	assert.Contains(t, stdout.String(), "HYPEMAN_DESKTOP_OK")

	_, err = guest.ExecIntoInstance(ctx, dialer, guest.ExecOptions{
		Command: []string{"cmd.exe"},
		TTY:     true,
		Session: guest.ExecSession_EXEC_SESSION_DESKTOP,
		Timeout: 30,
	})
	assert.ErrorContains(t, err, "desktop ConPTY sessions are not supported")

	source := filepath.Join(t.TempDir(), "roundtrip.txt")
	require.NoError(t, os.WriteFile(source, []byte("HYPEMAN_COPY_OK"), 0644))
	require.NoError(t, guest.CopyToInstance(ctx, dialer, guest.CopyToInstanceOptions{
		SrcPath: source,
		DstPath: `C:\ProgramData\Hypeman\roundtrip.txt`,
	}))
	destination := t.TempDir()
	require.NoError(t, guest.CopyFromInstance(ctx, dialer, guest.CopyFromInstanceOptions{
		SrcPath: `C:\ProgramData\Hypeman\roundtrip.txt`,
		DstPath: destination,
	}))
	contents, err := os.ReadFile(filepath.Join(destination, "roundtrip.txt"))
	require.NoError(t, err)
	assert.Equal(t, "HYPEMAN_COPY_OK", string(contents))
}

func assertWindowsNetworkReady(t *testing.T, ctx context.Context, manager *manager, instanceID, expectedIP string) {
	t.Helper()
	dialer, err := manager.GetVsockDialer(ctx, instanceID)
	require.NoError(t, err)
	allocation, err := manager.networkManager.GetAllocation(ctx, instanceID)
	require.NoError(t, err)
	expectedDNS := strings.Join(strings.FieldsFunc(allocation.DNS, func(r rune) bool { return r == ',' || r == ' ' }), ",")
	var stdout, stderr bytes.Buffer
	command := fmt.Sprintf("$a=Get-NetIPAddress -AddressFamily IPv4 | Where-Object IPAddress -eq '%s'; if (-not $a) { exit 20 }; $dns=@((Get-DnsClientServerAddress -InterfaceIndex $a.InterfaceIndex -AddressFamily IPv4).ServerAddresses); if (($dns -join ',') -ne '%s') { exit 21 }; [System.Net.Dns]::GetHostAddresses('example.com') | Out-Null; [Console]::Out.Write($a.IPAddress)", expectedIP, expectedDNS)
	exit, err := guest.ExecIntoInstance(ctx, dialer, guest.ExecOptions{
		Command: []string{"powershell.exe", "-NoProfile", "-NonInteractive", "-Command", command},
		Stdout:  &stdout,
		Stderr:  &stderr,
		Timeout: 30,
	})
	require.NoError(t, err, stderr.String())
	require.Equal(t, 0, exit.Code, stderr.String())
	assert.Equal(t, expectedIP, stdout.String())

	require.Eventually(t, func() bool {
		conn, err := net.DialTimeout("tcp", net.JoinHostPort(expectedIP, "3389"), time.Second)
		if err != nil {
			return false
		}
		_ = conn.Close()
		return true
	}, 2*time.Minute, time.Second, "RDP did not become reachable over the allocated network")

	ping := exec.Command("ping", "-c", "3", "-W", "2", expectedIP)
	require.NoError(t, ping.Run(), "allocated Windows IP did not answer ICMP")
}
