//go:build linux

package instances

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/kernel/hypeman/lib/hypervisor"
	"github.com/kernel/hypeman/lib/volumes"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const dockerInVMManualEnv = "HYPEMAN_RUN_DOCKER_IN_VM_TESTS"

func requireDockerInVMManualRun(t *testing.T) {
	t.Helper()
	if os.Getenv(dockerInVMManualEnv) != "1" {
		t.Skipf("set %s=1 to run docker-in-vm integration tests", dockerInVMManualEnv)
	}
}

func TestDockerInVMCloudHypervisorWithAttachedVolume(t *testing.T) {
	requireDockerInVMManualRun(t)
	requireKVMAccess(t)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()

	manager, _ := setupCompressionTestManagerForHypervisor(t, hypervisor.TypeCloudHypervisor)
	imageName := integrationTestImageRef(t, "docker.io/library/debian:12-slim")

	createImageAndWait(t, ctx, manager.imageManager, imageName)
	require.NoError(t, manager.systemManager.EnsureSystemFiles(ctx))

	volumeManager := volumes.NewManager(manager.paths, 0, nil)
	vol, err := volumeManager.CreateVolume(ctx, volumes.CreateVolumeRequest{
		Name:   "docker-data",
		SizeGb: 8,
	})
	require.NoError(t, err)

	var inst *Instance
	t.Cleanup(func() {
		if inst != nil {
			logInstanceArtifactsOnFailure(t, manager, inst.Id)
			if t.Failed() {
				if output, code, err := execCommand(context.Background(), inst, "sh", "-lc", "cat /tmp/dockerd.log || true"); err == nil {
					t.Logf("dockerd log (exit=%d):\n%s", code, output)
				}
			}
			_ = manager.DeleteInstance(context.Background(), inst.Id)
		}
		_ = volumeManager.DeleteVolume(context.Background(), vol.Id)
	})

	inst, err = manager.CreateInstance(ctx, CreateInstanceRequest{
		Name:           "docker-in-vm",
		Image:          imageName,
		Size:           4 * 1024 * 1024 * 1024,
		HotplugSize:    512 * 1024 * 1024,
		OverlaySize:    5 * 1024 * 1024 * 1024,
		Vcpus:          2,
		NetworkEnabled: true,
		Hypervisor:     hypervisor.TypeCloudHypervisor,
		Entrypoint:     []string{"/bin/sh", "-lc"},
		Cmd:            []string{"sleep infinity"},
		Volumes: []VolumeAttachment{
			{
				VolumeID:  vol.Id,
				MountPath: "/mnt/docker-data",
				Readonly:  false,
			},
		},
	})
	require.NoError(t, err)

	_, err = waitForInstanceState(ctx, manager, inst.Id, StateRunning, 60*time.Second)
	require.NoError(t, err)
	require.NoError(t, waitForExecAgent(ctx, manager, inst.Id, 60*time.Second))

	output, exitCode, err := execCommand(ctx, inst, "sh", "-lc", "findmnt -n -o FSTYPE,SOURCE /mnt/docker-data")
	require.NoError(t, err)
	require.Equal(t, 0, exitCode, "findmnt should succeed: %s", output)
	assert.Contains(t, output, "ext4", "docker data volume should be ext4-backed")
	assert.Contains(t, output, "/dev/vd", "docker data volume should come from an attached block device")

	output, exitCode, err = execCommand(ctx, inst, "sh", "-lc", `
set -eux
mkdir -p /var/lib/docker
mount --bind /mnt/docker-data /var/lib/docker
findmnt -n -o FSTYPE,SOURCE /var/lib/docker >/tmp/docker-bind-mount.txt
grep -q ext4 /tmp/docker-bind-mount.txt
`)
	require.NoError(t, err)
	require.Equal(t, 0, exitCode, "docker bind mount should work before docker install: %s", output)

	output, exitCode, err = execCommand(ctx, inst, "sh", "-lc", `
set -eux
export DEBIAN_FRONTEND=noninteractive
apt-get update
apt-get install -y docker.io curl
`)
	require.NoError(t, err)
	require.Equal(t, 0, exitCode, "docker install should succeed: %s", output)

	output, exitCode, err = execCommand(ctx, inst, "sh", "-lc", `
set -eux
nohup dockerd >/tmp/dockerd.log 2>&1 &
for i in $(seq 1 90); do
	if docker info >/tmp/docker-info.txt 2>/tmp/docker-info.err; then
		exit 0
	fi
	sleep 1
done
cat /tmp/docker-info.err || true
cat /tmp/dockerd.log || true
exit 1
`)
	require.NoError(t, err)
	require.Equal(t, 0, exitCode, "dockerd should become ready: %s", output)

	output, exitCode, err = execCommand(ctx, inst, "sh", "-lc", "docker info --format '{{.Driver}}'")
	require.NoError(t, err)
	require.Equal(t, 0, exitCode, "docker info should succeed: %s", output)
	assert.Equal(t, "overlay2", strings.TrimSpace(output), "docker should use overlay2 on the attached volume")

	output, exitCode, err = execCommand(ctx, inst, "sh", "-lc", "docker run --rm hello-world")
	require.NoError(t, err)
	require.Equal(t, 0, exitCode, "hello-world should run successfully: %s", output)
	assert.Contains(t, output, "Hello from Docker!", "hello-world output should confirm container execution")

	output, exitCode, err = execCommand(ctx, inst, "sh", "-lc", `
set -eux
docker rm -f docker-nginx >/dev/null 2>&1 || true
docker run -d --rm --name docker-nginx -p 8080:80 nginx:alpine
for i in $(seq 1 60); do
	if curl -fsS http://127.0.0.1:8080 >/tmp/docker-nginx.html; then
		grep -q 'Welcome to nginx!' /tmp/docker-nginx.html
		exit 0
	fi
	sleep 1
done
docker logs docker-nginx || true
exit 1
`)
	require.NoError(t, err)
	require.Equal(t, 0, exitCode, "docker port publishing should work: %s", output)
}
