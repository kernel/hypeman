//go:build linux && amd64

package instances

import (
	"bytes"
	"context"
	"os"
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

func TestWindowsStandbyRestoreIntegration(t *testing.T) {
	manager, _, image := setupWindowsSnapshotIntegration(t)
	ctx := context.Background()
	source := createWindowsSnapshotInstance(t, ctx, manager, image, "windows-restore-source")

	sourceMachineID := windowsMachineID(t, ctx, manager, source.Id)
	standby, err := manager.StandbyInstance(ctx, source.Id, StandbyInstanceRequest{})
	require.NoError(t, err)
	require.Equal(t, StateStandby, standby.State)
	restored, err := manager.RestoreInstance(ctx, source.Id)
	require.NoError(t, err)
	assert.Equal(t, sourceMachineID, windowsMachineID(t, ctx, manager, restored.Id), "same-instance restore must preserve Windows identity")

	_, err = manager.StopInstance(ctx, source.Id)
	require.NoError(t, err)
	snapshot, err := manager.CreateSnapshot(ctx, source.Id, CreateSnapshotRequest{
		Kind: SnapshotKindStopped,
		Name: "windows-stopped-snapshot",
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = manager.DeleteSnapshot(context.Background(), snapshot.Id) })
	_, err = manager.RestoreSnapshot(ctx, source.Id, snapshot.Id, RestoreSnapshotRequest{TargetState: StateStopped})
	require.NoError(t, err)
	forked, err := manager.ForkSnapshot(ctx, snapshot.Id, ForkSnapshotRequest{
		Name:        "windows-stopped-snapshot-child",
		TargetState: StateStopped,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = deleteTestInstanceNow(context.Background(), manager, forked.Id) })
	assert.True(t, forked.WindowsIdentityPending)
}

func TestWindowsStandbyForkIntegration(t *testing.T) {
	manager, p, image := setupWindowsSnapshotIntegration(t)
	ctx := context.Background()
	source := createWindowsSnapshotInstance(t, ctx, manager, image, "windows-standby-fork-source")
	sourceMachineID := windowsMachineID(t, ctx, manager, source.Id)
	sourceTPMEK := windowsTPMEK(t, ctx, manager, source.Id)
	windowsPowerShell(t, ctx, manager, source.Id, `New-Item -ItemType Directory -Force C:\ProgramData\Hypeman | Out-Null; Set-Content C:\ProgramData\Hypeman\standby.txt inherited`)

	_, err := manager.StandbyInstance(ctx, source.Id, StandbyInstanceRequest{})
	require.NoError(t, err)
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
}

func TestWindowsForkIntegration(t *testing.T) {
	manager, p, image := setupWindowsSnapshotIntegration(t)
	ctx := context.Background()
	source := createWindowsSnapshotInstance(t, ctx, manager, image, "windows-fork-source")
	sourceMachineID := windowsMachineID(t, ctx, manager, source.Id)
	sourceTPMEK := windowsTPMEK(t, ctx, manager, source.Id)
	windowsPowerShell(t, ctx, manager, source.Id, `New-Item -ItemType Directory -Force C:\ProgramData\Hypeman | Out-Null; Set-Content C:\ProgramData\Hypeman\identity.txt source`)

	stopped, err := manager.StopInstance(ctx, source.Id)
	require.NoError(t, err)
	require.Equal(t, StateStopped, stopped.State)
	forked, err := manager.ForkInstance(ctx, source.Id, ForkInstanceRequest{
		Name:        "windows-snapshot-child",
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

func setupWindowsSnapshotIntegration(t *testing.T) (*manager, *paths.Paths, *images.Image) {
	t.Helper()
	fixture := os.Getenv("HYPEMAN_WINDOWS_TEST_AGENT_PERSONA")
	if fixture == "" {
		fixture = "/ci/windows/persona-agent.qcow2"
	}
	if _, err := os.Stat(fixture); err != nil {
		if os.Getenv("CI") == "true" {
			t.Fatalf("required Windows snapshot fixture is missing: %s", fixture)
		}
		t.Skipf("Windows snapshot fixture is unavailable: %s", fixture)
	}
	acquireHeavyIO(t)

	manager, dataDir := setupTestManagerForQEMU(t)
	p := paths.New(dataDir)
	const digestHex = "acacacacacacacacacacacacacacacacacacacacacacacacacacacacacacacac"
	image := &images.Image{
		Name:     "registry.example/windows/persona:snapshot-integration",
		Digest:   "sha256:" + digestHex,
		Platform: "windows/amd64",
		Status:   images.StatusReady,
		Machine: &images.MachineImage{
			Kind:        images.MachineImageWindowsPersona,
			Base:        "registry.example/windows/base@sha256:cdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcd",
			TPM:         "2.0",
			SecureBoot:  "required",
			BitLocker:   "disabled",
			VirtualSize: 80 << 30,
		},
	}
	manager.imageManager = windowsFixtureImageManager{image: image}
	personaPath, err := images.GetMachineDiskPath(p, image.Name, image.Digest, image.Machine)
	require.NoError(t, err)
	require.NoError(t, forkvm.CopyRegularFile(fixture, personaPath))
	require.NoError(t, os.Chmod(personaPath, 0444))
	return manager, p, image
}

func createWindowsSnapshotInstance(t *testing.T, ctx context.Context, manager *manager, image *images.Image, name string) *Instance {
	t.Helper()
	instance, err := manager.CreateInstance(ctx, CreateInstanceRequest{
		Name:       name,
		Image:      image.Name,
		Platform:   "windows/amd64",
		Size:       4 << 30,
		Vcpus:      4,
		Hypervisor: hypervisor.TypeQEMU,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = deleteTestInstanceNow(context.Background(), manager, instance.Id) })
	require.Eventually(t, func() bool {
		current, err := manager.GetInstance(ctx, instance.Id)
		return err == nil && current.State == StateRunning
	}, 75*time.Second, 500*time.Millisecond)
	return instance
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
