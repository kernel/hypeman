//go:build linux && amd64

package instances

import (
	"bytes"
	"context"
	"fmt"
	"os"
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
	source := createWindowsLifecycleInstance(t, ctx, manager, image, "windows-lifecycle-source", true)

	sourceMachineID := windowsMachineID(t, ctx, manager, source.Id, `New-Item -ItemType Directory -Force C:\ProgramData\Hypeman | Out-Null; Set-Content C:\ProgramData\Hypeman\standby.txt inherited`)

	standby, err := manager.StandbyInstance(ctx, source.Id, StandbyInstanceRequest{})
	require.NoError(t, err)
	require.Equal(t, StateStandby, standby.State)
	forked, err := manager.ForkInstance(ctx, source.Id, ForkInstanceRequest{
		Name:        "windows-standby-child",
		TargetState: StateStandby,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = deleteTestInstanceNow(context.Background(), manager, forked.Id) })
	assert.Equal(t, source.VsockCID, forked.VsockCID, "memory-restored Windows forks retain the captured VioSock CID until cold boot")
	assert.Equal(t, regularFileContents(t, p.InstanceTPMDir(source.Id)), regularFileContents(t, p.InstanceTPMDir(forked.Id)), "memory forks inherit TPM state")
	assertIndependentFile(t, p.InstanceOVMFVars(source.Id), p.InstanceOVMFVars(forked.Id))
	assertIndependentFile(t, p.InstanceWindowsDisk(source.Id), p.InstanceWindowsDisk(forked.Id))

	restoredFork, err := manager.RestoreInstance(ctx, forked.Id)
	require.NoError(t, err)
	waitForWindowsRunning(t, ctx, manager, restoredFork.Id)
	forkedMachineID, inherited := windowsMachineIDAndFile(t, ctx, manager, restoredFork.Id, `C:\ProgramData\Hypeman\standby.txt`)
	assert.NotEqual(t, sourceMachineID, forkedMachineID)
	assert.Equal(t, "inherited", inherited)
}

func TestWindowsStoppedForkIntegration(t *testing.T) {
	manager, p, image := setupWindowsLifecycleIntegration(t)
	ctx := context.Background()
	source := createWindowsLifecycleInstance(t, ctx, manager, image, "windows-stopped-fork-source", false)
	sourceMachineID := windowsMachineID(t, ctx, manager, source.Id, `New-Item -ItemType Directory -Force C:\ProgramData\Hypeman | Out-Null; Set-Content C:\ProgramData\Hypeman\identity.txt source`)

	stopped, err := manager.StopInstance(ctx, source.Id)
	require.NoError(t, err)
	require.Equal(t, StateStopped, stopped.State)
	sourceTPM := regularFileContents(t, p.InstanceTPMDir(source.Id))
	forked, err := manager.ForkInstance(ctx, source.Id, ForkInstanceRequest{
		Name:        "windows-stopped-child",
		TargetState: StateStopped,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = deleteTestInstanceNow(context.Background(), manager, forked.Id) })
	forkedTPM := regularFileContents(t, p.InstanceTPMDir(forked.Id))
	assert.NotEmpty(t, sourceTPM)
	assert.Empty(t, forkedTPM, "stopped forks clear copied TPM state before cold boot")
	assert.NotEqual(t, source.VsockCID, forked.VsockCID, "forks need unique vsock CIDs")
	assertIndependentFile(t, p.InstanceOVMFVars(source.Id), p.InstanceOVMFVars(forked.Id))
	assertIndependentFile(t, p.InstanceWindowsDisk(source.Id), p.InstanceWindowsDisk(forked.Id))

	started, err := manager.StartInstance(ctx, forked.Id, StartInstanceRequest{})
	require.NoError(t, err)
	waitForWindowsRunning(t, ctx, manager, started.Id)
	forkedMachineID := windowsMachineID(t, ctx, manager, started.Id, "")
	assert.NotEqual(t, sourceMachineID, forkedMachineID, "fork must receive a new Windows machine identity")

	sourceDiskBefore, err := os.Stat(p.InstanceWindowsDisk(source.Id))
	require.NoError(t, err)
	assert.Equal(t, "child", windowsPowerShell(t, ctx, manager, started.Id, `Set-Content C:\ProgramData\Hypeman\identity.txt child; Get-Content C:\ProgramData\Hypeman\identity.txt`))
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

func windowsMachineID(t *testing.T, ctx context.Context, manager *manager, instanceID, prelude string) string {
	t.Helper()
	command := `[Console]::Out.Write((Get-ItemProperty 'HKLM:\SOFTWARE\Microsoft\Cryptography').MachineGuid)`
	if prelude != "" {
		command = prelude + "; " + command
	}
	machineID := windowsPowerShell(t, ctx, manager, instanceID, command)
	require.NotEmpty(t, machineID, "Windows machine identity")
	return machineID
}

func windowsMachineIDAndFile(t *testing.T, ctx context.Context, manager *manager, instanceID, path string) (string, string) {
	t.Helper()
	command := fmt.Sprintf(`$machine=(Get-ItemProperty 'HKLM:\SOFTWARE\Microsoft\Cryptography').MachineGuid; $content=Get-Content '%s'; [Console]::Out.Write($machine + "|" + $content)`, path)
	parts := strings.Split(windowsPowerShell(t, ctx, manager, instanceID, command), "|")
	require.Len(t, parts, 2)
	require.NotEmpty(t, parts[0], "Windows machine identity")
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

func regularFileContents(t *testing.T, root string) map[string]string {
	t.Helper()
	files := make(map[string]string)
	require.NoError(t, filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.Type().IsRegular() {
			return nil
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		contents, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		files[relative] = string(contents)
		return nil
	}))
	return files
}

func assertIndependentFile(t *testing.T, source, fork string) {
	t.Helper()
	sourceInfo, err := os.Stat(source)
	require.NoError(t, err)
	forkInfo, err := os.Stat(fork)
	require.NoError(t, err)
	assert.False(t, os.SameFile(sourceInfo, forkInfo), "%s and %s must not share an inode", source, fork)
}
