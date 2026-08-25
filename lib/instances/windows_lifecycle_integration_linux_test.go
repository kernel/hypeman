//go:build linux && amd64

package instances

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
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
	source := createWindowsLifecycleInstance(t, ctx, manager, image, "windows-lifecycle-source", true)

	sourceMachineID, sourceTPMEK := windowsIdentity(t, ctx, manager, source.Id, `New-Item -ItemType Directory -Force C:\ProgramData\Hypeman | Out-Null; Set-Content C:\ProgramData\Hypeman\standby.txt inherited`)

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
	forkedMachineID, forkedTPMEK := windowsIdentity(t, ctx, manager, forked.Id, "")
	assert.NotEqual(t, sourceMachineID, forkedMachineID)
	assert.Equal(t, sourceTPMEK, forkedTPMEK, "memory forks inherit TPM state from the QEMU migration stream")
	assert.Equal(t, "inherited", windowsPowerShell(t, ctx, manager, forked.Id, `Get-Content C:\ProgramData\Hypeman\standby.txt`))
	assertIndependentFile(t, p.InstanceOVMFVars(source.Id), p.InstanceOVMFVars(forked.Id))
	assertIndependentFile(t, p.InstanceWindowsDisk(source.Id), p.InstanceWindowsDisk(forked.Id))

	stoppedFork, err := manager.StopInstance(ctx, forked.Id)
	require.NoError(t, err)
	require.Equal(t, StateStopped, stoppedFork.State)
	restored, err := manager.RestoreInstance(ctx, source.Id)
	require.NoError(t, err)
	waitForWindowsRunning(t, ctx, manager, restored.Id)
	restoredMachineID, restoredTPMEK := windowsIdentity(t, ctx, manager, restored.Id, "")
	assert.Equal(t, sourceMachineID, restoredMachineID, "same-instance restore must preserve Windows identity")
	assert.Equal(t, sourceTPMEK, restoredTPMEK, "same-instance restore must preserve TPM identity")
	assertWindowsNetworkReady(t, ctx, manager, restored.Id, source.IP)
	assertWindowsGuestControl(t, ctx, manager, restored.Id)
}

func TestWindowsStoppedForkIntegration(t *testing.T) {
	manager, p, image := setupWindowsLifecycleIntegration(t)
	ctx := context.Background()
	source := createWindowsLifecycleInstance(t, ctx, manager, image, "windows-stopped-fork-source", false)
	sourceMachineID, sourceTPMEK := windowsIdentity(t, ctx, manager, source.Id, `New-Item -ItemType Directory -Force C:\ProgramData\Hypeman | Out-Null; Set-Content C:\ProgramData\Hypeman\identity.txt source`)

	stopped, err := manager.StopInstance(ctx, source.Id)
	require.NoError(t, err)
	require.Equal(t, StateStopped, stopped.State)
	forked, err := manager.ForkInstance(ctx, source.Id, ForkInstanceRequest{
		Name:        "windows-stopped-child",
		TargetState: StateRunning,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = deleteTestInstanceNow(context.Background(), manager, forked.Id) })
	forkedMachineID, forkedTPMEK := windowsIdentity(t, ctx, manager, forked.Id, "")
	assert.NotEqual(t, sourceMachineID, forkedMachineID, "fork must receive a new Windows machine identity")
	assert.NotEqual(t, sourceTPMEK, forkedTPMEK, "cold forks must initialize a fresh TPM")
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

func createWindowsLifecycleInstance(t *testing.T, ctx context.Context, manager *manager, image *images.Image, name string, networkEnabled bool) *Instance {
	t.Helper()
	instance, err := manager.CreateInstance(ctx, CreateInstanceRequest{
		Name:           name,
		Image:          image.Name,
		Platform:       "windows/amd64",
		Size:           4 << 30,
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
	exit, err := guest.ExecIntoInstance(ctx, dialer, guest.ExecOptions{
		Command: []string{"cmd.exe", "/d", "/c", "echo HYPEMAN_GUEST_CONTROL_OK"},
		Stdout:  &stdout,
		Stderr:  &stderr,
		Timeout: 15,
	})
	require.NoError(t, err, stderr.String())
	require.Equal(t, 0, exit.Code)
	assert.Contains(t, stdout.String(), "HYPEMAN_GUEST_CONTROL_OK")
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

func windowsIdentity(t *testing.T, ctx context.Context, manager *manager, instanceID, prelude string) (string, string) {
	t.Helper()
	command := `$machine=(Get-ItemProperty 'HKLM:\SOFTWARE\Microsoft\Cryptography').MachineGuid; $tpm=(Get-TpmEndorsementKeyInfo -HashAlgorithm Sha256).PublicKeyHash; [Console]::Out.Write($machine + "|" + $tpm)`
	if prelude != "" {
		command = prelude + "; " + command
	}
	parts := strings.Split(windowsPowerShell(t, ctx, manager, instanceID, command), "|")
	require.Len(t, parts, 2)
	require.NotEmpty(t, parts[0], "Windows machine identity")
	require.NotEmpty(t, parts[1], "TPM endorsement key hash")
	return parts[0], parts[1]
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
