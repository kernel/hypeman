package integration

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/kernel/hypeman/cmd/api/config"
	"github.com/kernel/hypeman/lib/devices"
	"github.com/kernel/hypeman/lib/images"
	"github.com/kernel/hypeman/lib/instances"
	"github.com/kernel/hypeman/lib/network"
	"github.com/kernel/hypeman/lib/paths"
	"github.com/kernel/hypeman/lib/system"
	"github.com/kernel/hypeman/lib/volumes"
	"github.com/stretchr/testify/require"
)

// Keep in sync with defaultImages in cmd/test-prewarm/main.go.
const nestedDockerImage = "docker.io/library/docker:28.5.2-dind"

// nestedDockerExecScript starts a container from a busybox-only image built
// out of the dind rootfs (no registry access needed), then checks that
// `docker exec` sees the container rootfs rather than the guest's initrd:
// the file PID 1 wrote must be visible from exec, and a file written from
// exec must land in the container rootfs as seen through /proc/<pid>/root.
const nestedDockerExecScript = `set -eux
exec 2>&1
arch=$(uname -m)
tar -cf /tmp/rootfs.tar -C / bin/busybox lib/ld-musl-$arch.so.1 lib/libc.musl-$arch.so.1
docker import /tmp/rootfs.tar nested:test
docker run -d --name nested --network none nested:test \
  /bin/busybox sh -c 'echo pid1-created >/runtime-created; exec /bin/busybox sleep 300'
pid=$(docker inspect --format '{{.State.Pid}}' nested)
for _ in $(seq 1 50); do
  test -f "/proc/$pid/root/runtime-created" && break
  sleep 0.1
done
test "$(docker exec nested /bin/busybox cat /runtime-created)" = pid1-created
docker exec nested /bin/busybox sh -c 'echo exec-created >/exec-created'
test "$(cat "/proc/$pid/root/exec-created")" = exec-created
`

// TestNestedDockerExecRoot boots a Docker-in-Docker image with dockerd as the
// entrypoint and verifies that stock runc's `docker exec` enters the nested
// container's rootfs. This regressed when init only chroot(2)ed into the image
// rootfs and left the initrd as the mount namespace root.
func TestNestedDockerExecRoot(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	if _, err := os.Stat("/dev/kvm"); os.IsNotExist(err) {
		t.Skip("/dev/kvm not available")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	tmpDir := t.TempDir()
	p := paths.New(tmpDir)
	cfg := &config.Config{
		DataDir: tmpDir,
		Network: newParallelTestNetworkConfig(t),
	}

	imageManager, err := images.NewManager(p, 1, nil)
	require.NoError(t, err)
	systemManager := system.NewManager(p)
	networkManager := network.NewManager(p, cfg, nil)
	deviceManager := devices.NewManager(p)
	volumeManager := volumes.NewManager(p, 0, nil)
	limits := instances.ResourceLimits{MaxOverlaySize: 100 * 1024 * 1024 * 1024}
	instanceManager := instances.NewManager(p, imageManager, systemManager, networkManager, deviceManager, volumeManager, limits, "", instances.SnapshotPolicy{}, nil, nil)

	imageName := integrationTestImageRef(t, nestedDockerImage)
	_, err = imageManager.CreateImage(ctx, images.CreateImageRequest{Name: imageName})
	require.NoError(t, err)
	require.NoError(t, imageManager.WaitForReady(ctx, imageName))
	require.NoError(t, systemManager.EnsureSystemFiles(ctx))

	inst, err := instanceManager.CreateInstance(ctx, instances.CreateInstanceRequest{
		Name:        "nested-docker-test",
		Image:       imageName,
		Size:        1024 * 1024 * 1024,
		OverlaySize: 1024 * 1024 * 1024,
		Vcpus:       2,
		Entrypoint:  []string{"dockerd"},
		// vfs: the guest rootfs is already overlayfs, which cannot back another overlay2.
		// No bridge: the instance has no network and the nested container does not need one.
		Cmd: []string{"--host=unix:///var/run/docker.sock", "--storage-driver=vfs", "--bridge=none", "--iptables=false", "--ip6tables=false"},
	})
	require.NoError(t, err)
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cleanupCancel()
		if err := instanceManager.DeleteInstance(cleanupCtx, inst.Id); err != nil {
			t.Errorf("delete nested docker test instance during cleanup: %v", err)
		}
	})

	require.NoError(t, waitForGuestAgent(ctx, instanceManager, inst.Id, 60*time.Second))

	var lastOutput string
	require.Eventually(t, func() bool {
		output, exitCode, err := execInInstance(ctx, inst, "docker", "info")
		lastOutput = output
		return err == nil && exitCode == 0
	}, 60*time.Second, time.Second, "dockerd did not become ready: %s", lastOutput)

	output, exitCode, err := execInInstance(ctx, inst, "sh", "-c", nestedDockerExecScript)
	require.NoError(t, err)
	require.Equalf(t, 0, exitCode, "docker exec did not use the container rootfs:\n%s", strings.TrimSpace(output))
}
