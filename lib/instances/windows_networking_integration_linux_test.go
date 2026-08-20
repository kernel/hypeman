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

func TestWindowsNetworkingIntegration(t *testing.T) {
	fixture := os.Getenv("HYPEMAN_WINDOWS_TEST_AGENT_PERSONA")
	if fixture == "" {
		fixture = "/ci/windows/persona-agent.qcow2"
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
		Name:     "registry.example/windows/persona:networking-integration",
		Digest:   "sha256:" + digestHex,
		Platform: "windows/amd64",
		Status:   images.StatusReady,
		Machine: &images.MachineImage{
			Kind:        images.MachineImageWindowsPersona,
			Base:        "registry.example/windows/base@sha256:cdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcd",
			TPM:         "2.0",
			SecureBoot:  "required",
			VirtualSize: 80 << 30,
		},
	}
	manager.imageManager = windowsFixtureImageManager{image: image}
	personaPath, err := images.GetMachineDiskPath(p, image.Name, image.Digest, image.Machine)
	require.NoError(t, err)
	require.NoError(t, forkvm.CopyRegularFile(fixture, personaPath))
	require.NoError(t, os.Chmod(personaPath, 0444))

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

	assertWindowsNetworkReady(t, ctx, manager, instance.Id, instance.IP)
}

func assertWindowsNetworkReady(t *testing.T, ctx context.Context, manager *manager, instanceID, expectedIP string) {
	t.Helper()
	require.Eventually(t, func() bool {
		current, err := manager.GetInstance(ctx, instanceID)
		return err == nil && current.State == StateRunning
	}, 4*time.Minute, time.Second)

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
