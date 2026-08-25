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
	manager, p, image := setupWindowsLifecycleIntegration(t)
	ctx := context.Background()
	source := createWindowsLifecycleInstance(t, ctx, manager, image, "windows-lifecycle-source", true, 8<<30)

	assertWindowsNetworkReady(t, ctx, manager, source.Id, source.IP)
	assertWindowsGuestControl(t, ctx, manager, source.Id)
	sourceMachineID := windowsMachineID(t, ctx, manager, source.Id)
	sourceTPMEK := windowsTPMEK(t, ctx, manager, source.Id)
	windowsPowerShell(t, ctx, manager, source.Id, `New-Item -ItemType Directory -Force C:\ProgramData\Hypeman | Out-Null; Set-Content C:\ProgramData\Hypeman\standby.txt inherited`)

	standby, err := manager.StandbyInstance(ctx, source.Id, StandbyInstanceRequest{})
	require.NoError(t, err)
	require.Equal(t, StateStandby, standby.State)
	forked, err := manager.ForkInstance(ctx, source.Id, ForkInstanceRequest{
		Name:        "windows-standby-child",
		TargetState: StateRunning,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = deleteTestInstanceNow(context.Background(), manager, forked.Id) })
	assert.Equal(t, source.VsockCID, forked.VsockCID, "memory-restored Windows forks retain the captured VioSock CID until cold boot")
	assert.NotEqual(t, sourceMachineID, windowsMachineID(t, ctx, manager, forked.Id))
	assert.Equal(t, sourceTPMEK, windowsTPMEK(t, ctx, manager, forked.Id), "memory forks inherit TPM state from the QEMU migration stream")
	assert.Equal(t, "inherited", windowsPowerShell(t, ctx, manager, forked.Id, `Get-Content C:\ProgramData\Hypeman\standby.txt`))
	assertIndependentFile(t, p.InstanceOVMFVars(source.Id), p.InstanceOVMFVars(forked.Id))
	assertIndependentFile(t, p.InstanceWindowsDisk(source.Id), p.InstanceWindowsDisk(forked.Id))

	stoppedFork, err := manager.StopInstance(ctx, forked.Id)
	require.NoError(t, err)
	require.Equal(t, StateStopped, stoppedFork.State)
	restored, err := manager.RestoreInstance(ctx, source.Id)
	require.NoError(t, err)
	waitForWindowsRunning(t, ctx, manager, restored.Id)
	assert.Equal(t, sourceMachineID, windowsMachineID(t, ctx, manager, restored.Id), "same-instance restore must preserve Windows identity")
	assertWindowsNetworkReady(t, ctx, manager, restored.Id, source.IP)
	assertWindowsCopyRoundTrip(t, ctx, manager, restored.Id)
}

func TestWindowsStoppedForkIntegration(t *testing.T) {
	manager, p, image := setupWindowsLifecycleIntegration(t)
	ctx := context.Background()
	source := createWindowsLifecycleInstance(t, ctx, manager, image, "windows-stopped-fork-source", false, 4<<30)
	sourceMachineID := windowsMachineID(t, ctx, manager, source.Id)
	sourceTPMEK := windowsTPMEK(t, ctx, manager, source.Id)
	windowsPowerShell(t, ctx, manager, source.Id, `New-Item -ItemType Directory -Force C:\ProgramData\Hypeman | Out-Null; Set-Content C:\ProgramData\Hypeman\identity.txt source`)

	stopped, err := manager.StopInstance(ctx, source.Id)
	require.NoError(t, err)
	require.Equal(t, StateStopped, stopped.State)
	forked, err := manager.ForkInstance(ctx, source.Id, ForkInstanceRequest{
		Name:        "windows-stopped-child",
		TargetState: StateRunning,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = deleteTestInstanceNow(context.Background(), manager, forked.Id) })
	assert.NotEqual(t, sourceMachineID, windowsMachineID(t, ctx, manager, forked.Id), "fork must receive a new Windows machine identity")
	assert.NotEqual(t, sourceTPMEK, windowsTPMEK(t, ctx, manager, forked.Id), "cold forks must initialize a fresh TPM")
	assert.NotEqual(t, source.VsockCID, forked.VsockCID, "forks need unique vsock CIDs")
	assertIndependentFile(t, p.InstanceOVMFVars(source.Id), p.InstanceOVMFVars(forked.Id))
	assertIndependentFile(t, p.InstanceWindowsDisk(source.Id), p.InstanceWindowsDisk(forked.Id))
	require.DirExists(t, p.InstanceTPMDir(forked.Id))

	sourceDiskBefore, err := os.Stat(p.InstanceWindowsDisk(source.Id))
	require.NoError(t, err)
	windowsPowerShell(t, ctx, manager, forked.Id, `Set-Content C:\ProgramData\Hypeman\identity.txt child`)
	assert.Equal(t, "child", windowsPowerShell(t, ctx, manager, forked.Id, `Get-Content C:\ProgramData\Hypeman\identity.txt`))
	sourceDiskAfter, err := os.Stat(p.InstanceWindowsDisk(source.Id))
	require.NoError(t, err)
	assert.Equal(t, sourceDiskBefore.ModTime(), sourceDiskAfter.ModTime(), "child writes must not modify the stopped source disk")
}

func setupWindowsLifecycleIntegration(t *testing.T) (*manager, *paths.Paths, *images.Image) {
	t.Helper()
	if os.Getenv("HYPEMAN_RUN_WINDOWS_LIFECYCLE_INTEGRATION") != "1" {
		t.Skip("run by the dedicated Windows lifecycle CI gates")
	}
	fixture := os.Getenv("HYPEMAN_WINDOWS_TEST_AGENT_IMAGE")
	if fixture == "" {
		fixture = "/ci/windows/image-agent.qcow2"
	}
	if _, err := os.Stat(fixture); err != nil {
		if os.Getenv("CI") == "true" {
			t.Fatalf("required Windows lifecycle fixture is missing: %s", fixture)
		}
		t.Skipf("Windows lifecycle fixture is unavailable: %s", fixture)
	}
	acquireHeavyIO(t)

	manager, dataDir := setupTestManagerForQEMU(t)
	p := paths.New(dataDir)
	const digestHex = "acacacacacacacacacacacacacacacacacacacacacacacacacacacacacacacac"
	image := &images.Image{
		Name:     "registry.example/windows/image:lifecycle-integration",
		Digest:   "sha256:" + digestHex,
		Platform: "windows/amd64",
		Status:   images.StatusReady,
		Machine: &images.MachineImage{
			Kind:        images.MachineImageWindowsImage,
			Base:        "registry.example/windows/base@sha256:cdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcd",
			TPM:         "2.0",
			SecureBoot:  "required",
			BitLocker:   "disabled",
			VirtualSize: 80 << 30,
		},
	}
	manager.imageManager = windowsFixtureImageManager{image: image}
	imagePath, err := images.GetMachineDiskPath(p, image.Name, image.Digest, image.Machine)
	require.NoError(t, err)
	require.NoError(t, forkvm.CopyRegularFile(fixture, imagePath))
	require.NoError(t, os.Chmod(imagePath, 0444))
	return manager, p, image
}

func createWindowsLifecycleInstance(t *testing.T, ctx context.Context, manager *manager, image *images.Image, name string, networkEnabled bool, size int64) *Instance {
	t.Helper()
	instance, err := manager.CreateInstance(ctx, CreateInstanceRequest{
		Name:           name,
		Image:          image.Name,
		Platform:       "windows/amd64",
		Size:           size,
		Vcpus:          4,
		NetworkEnabled: networkEnabled,
		Hypervisor:     hypervisor.TypeQEMU,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = deleteTestInstanceNow(context.Background(), manager, instance.Id) })
	waitForWindowsRunning(t, ctx, manager, instance.Id)
	return instance
}

func waitForWindowsRunning(t *testing.T, ctx context.Context, manager *manager, instanceID string) {
	t.Helper()
	require.Eventually(t, func() bool {
		current, err := manager.GetInstance(ctx, instanceID)
		return err == nil && current.State == StateRunning
	}, 75*time.Second, 500*time.Millisecond)
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

	time.Sleep(time.Second)
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

}

func assertWindowsCopyRoundTrip(t *testing.T, ctx context.Context, manager *manager, instanceID string) {
	t.Helper()
	dialer, err := manager.GetVsockDialer(ctx, instanceID)
	require.NoError(t, err)
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

func windowsMachineID(t *testing.T, ctx context.Context, manager *manager, instanceID string) string {
	t.Helper()
	return windowsPowerShell(t, ctx, manager, instanceID, `(Get-ItemProperty 'HKLM:\SOFTWARE\Microsoft\Cryptography').MachineGuid`)
}

func windowsTPMEK(t *testing.T, ctx context.Context, manager *manager, instanceID string) string {
	t.Helper()
	hash := windowsPowerShell(t, ctx, manager, instanceID, `(Get-TpmEndorsementKeyInfo -HashAlgorithm Sha256).PublicKeyHash`)
	require.NotEmpty(t, hash, "TPM endorsement key hash")
	return hash
}

func windowsPowerShell(t *testing.T, ctx context.Context, manager *manager, instanceID, command string) string {
	t.Helper()
	dialer, err := manager.GetVsockDialer(ctx, instanceID)
	require.NoError(t, err)
	var stdout, stderr bytes.Buffer
	exit, err := guest.ExecIntoInstance(ctx, dialer, guest.ExecOptions{
		Command:      []string{"powershell.exe", "-NoProfile", "-NonInteractive", "-Command", command},
		Stdout:       &stdout,
		Stderr:       &stderr,
		WaitForAgent: 30 * time.Second,
		Timeout:      30,
	})
	require.NoError(t, err, stderr.String())
	require.Equal(t, 0, exit.Code, stderr.String())
	return string(bytes.TrimSpace(stdout.Bytes()))
}

func assertIndependentFile(t *testing.T, source, fork string) {
	t.Helper()
	sourceInfo, err := os.Stat(source)
	require.NoError(t, err)
	forkInfo, err := os.Stat(fork)
	require.NoError(t, err)
	assert.False(t, os.SameFile(sourceInfo, forkInfo), "%s and %s must not share an inode", source, fork)
}
