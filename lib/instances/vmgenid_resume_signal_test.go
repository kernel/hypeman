//go:build linux

package instances

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/kernel/hypeman/lib/hypervisor"
	"github.com/kernel/hypeman/lib/images"
	"github.com/kernel/hypeman/lib/paths"
	snapshottest "github.com/kernel/hypeman/lib/snapshot/testsupport"
	"github.com/kernel/hypeman/lib/system"
	"github.com/stretchr/testify/require"
)

const vmgenidResumeSignalEnv = "HYPEMAN_RUN_VMGENID_RESUME_SIGNAL"

func TestFirecrackerVMGenIDResumeSignal(t *testing.T) {
	if strings.TrimSpace(os.Getenv(vmgenidResumeSignalEnv)) != "1" {
		t.Skipf("set %s=1 to run Firecracker VMGenID resume signal probe", vmgenidResumeSignalEnv)
	}
	requireFirecrackerIntegrationPrereqs(t)

	mgr, tmpDir := setupTestManagerForFirecrackerNoNetwork(t)
	ctx := context.Background()
	p := paths.New(tmpDir)

	imageManager, err := images.NewManager(p, 1, nil)
	require.NoError(t, err)
	imageName := integrationTestImageRef(t, "docker.io/library/alpine:latest")
	snapshottest.EnsureImageReady(t, ctx, p, imageManager, imageName)

	systemManager := system.NewManager(p)
	require.NoError(t, systemManager.EnsureSystemFiles(ctx))

	source, err := mgr.CreateInstance(ctx, CreateInstanceRequest{
		Name:        "fc-vmgenid-src",
		Image:       imageName,
		Size:        1024 * 1024 * 1024,
		OverlaySize: 1024 * 1024 * 1024,
		Vcpus:       1,
		Hypervisor:  hypervisor.TypeFirecracker,
		Cmd:         []string{"sleep", "infinity"},
	})
	require.NoError(t, err)
	sourceID := source.Id
	t.Cleanup(func() { _ = mgr.DeleteInstance(context.Background(), sourceID) })

	source, err = waitForInstanceState(ctx, mgr, sourceID, StateRunning, integrationTestTimeout(45*time.Second))
	require.NoError(t, err)
	require.NoError(t, waitForExecAgent(ctx, mgr, sourceID, 45*time.Second))

	sourceOut, _, err := execCommandWithRetry(ctx, source, 20*time.Second, "sh", "-c", vmgenidProbeCommand())
	require.NoError(t, err)
	t.Logf("source vmgenid probe:\n%s", sourceOut)
	require.Contains(t, sourceOut, "CONFIG_VMGENID=y")

	snapshot, err := mgr.CreateSnapshot(ctx, sourceID, CreateSnapshotRequest{
		Kind: SnapshotKindStandby,
		Name: "fc-vmgenid-snap",
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = mgr.DeleteSnapshot(context.Background(), snapshot.Id) })

	fork, err := mgr.ForkSnapshot(ctx, snapshot.Id, ForkSnapshotRequest{
		Name:        "fc-vmgenid-fork",
		TargetState: StateRunning,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = mgr.DeleteInstance(context.Background(), fork.Id) })
	require.Equal(t, StateRunning, fork.State)
	require.NoError(t, waitForExecAgent(ctx, mgr, fork.Id, 45*time.Second))

	forkOut, _, err := execCommandWithRetry(ctx, fork, 20*time.Second, "sh", "-c", vmgenidProbeCommand())
	require.NoError(t, err)
	t.Logf("fork vmgenid probe:\n%s", forkOut)
	require.Contains(t, forkOut, "CONFIG_VMGENID=y")
	require.Contains(t, forkOut, "crng reseeded due to virtual machine fork")
}

func vmgenidProbeCommand() string {
	return strings.Join([]string{
		"uname -r",
		"zcat /proc/config.gz | grep -E '^CONFIG_(VIRT_DRIVERS|VMGENID)='",
		"ls -ld /sys/devices/platform/FCVMGID:00 2>/dev/null || true",
		"dmesg | grep -E 'crng reseeded due to virtual machine fork|vmgenid|VMGenID' || true",
	}, "; ")
}
