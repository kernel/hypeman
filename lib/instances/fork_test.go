package instances

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kernel/hypeman/lib/guest"
	"github.com/kernel/hypeman/lib/hypervisor"
	"github.com/kernel/hypeman/lib/images"
	"github.com/kernel/hypeman/lib/paths"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestForkInstance_VZStoppedSourceSupported(t *testing.T) {
	manager, _ := setupTestManager(t)
	ctx := context.Background()
	if _, err := manager.getVMStarter(hypervisor.TypeVZ); err != nil {
		t.Skip("vz starter not available on this platform")
	}

	sourceID := "fork-vz-source"
	require.NoError(t, manager.ensureDirectories(sourceID))

	meta := &metadata{StoredMetadata: StoredMetadata{
		Id:                sourceID,
		Name:              "fork-vz-source",
		Image:             "docker.io/library/alpine:latest",
		CreatedAt:         time.Now(),
		HypervisorType:    hypervisor.TypeVZ,
		HypervisorVersion: "test",
		SocketPath:        paths.New(manager.paths.DataDir()).InstanceSocket(sourceID, "vz.sock"),
		DataDir:           paths.New(manager.paths.DataDir()).InstanceDir(sourceID),
		VsockCID:          42,
		VsockSocket:       paths.New(manager.paths.DataDir()).InstanceVsockSocket(sourceID),
	}}
	require.NoError(t, manager.saveMetadata(meta))

	forked, err := manager.ForkInstance(ctx, sourceID, ForkInstanceRequest{Name: "fork-vz-copy"})
	require.NoError(t, err)
	require.NotNil(t, forked)
	assert.Equal(t, StateStopped, forked.State)
	assert.Equal(t, hypervisor.TypeVZ, forked.HypervisorType)
	assert.NotEqual(t, sourceID, forked.Id)
}

func TestResolveForkTargetState_DefaultsToSourceState(t *testing.T) {
	tests := []struct {
		name   string
		source State
		want   State
	}{
		{name: "running", source: StateRunning, want: StateRunning},
		{name: "standby", source: StateStandby, want: StateStandby},
		{name: "stopped", source: StateStopped, want: StateStopped},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveForkTargetState("", tc.source)
			require.NoError(t, err)
			require.Equal(t, tc.want, got)
		})
	}
}

func TestValidateForkRequest_InvalidTargetState(t *testing.T) {
	err := validateForkRequest(ForkInstanceRequest{
		Name:        "fork-invalid-target",
		TargetState: State("Created"),
	})
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrInvalidRequest)
}

func TestValidateForkVolumeSafety(t *testing.T) {
	err := validateForkVolumeSafety([]VolumeAttachment{
		{VolumeID: "vol-rw", MountPath: "/data", Readonly: false},
	})
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrNotSupported)

	err = validateForkVolumeSafety([]VolumeAttachment{
		{VolumeID: "vol-ro", MountPath: "/data", Readonly: true, Overlay: false},
		{VolumeID: "vol-cow", MountPath: "/tmp", Readonly: true, Overlay: true},
	})
	require.NoError(t, err)
}

func TestCleanupForkInstanceOnError(t *testing.T) {
	manager, _ := setupTestManager(t)
	ctx := context.Background()

	forkID := "fork-cleanup-target"
	require.NoError(t, manager.ensureDirectories(forkID))

	meta := &metadata{StoredMetadata: StoredMetadata{
		Id:                forkID,
		Name:              "fork-cleanup-target",
		Image:             "docker.io/library/alpine:latest",
		CreatedAt:         time.Now(),
		HypervisorType:    hypervisor.TypeCloudHypervisor,
		HypervisorVersion: "test",
		SocketPath:        paths.New(manager.paths.DataDir()).InstanceSocket(forkID, "cloud-hypervisor.sock"),
		DataDir:           paths.New(manager.paths.DataDir()).InstanceDir(forkID),
		VsockCID:          43,
		VsockSocket:       paths.New(manager.paths.DataDir()).InstanceVsockSocket(forkID),
	}}
	require.NoError(t, manager.saveMetadata(meta))

	require.DirExists(t, meta.DataDir)
	require.NoError(t, manager.cleanupForkInstanceOnError(ctx, forkID))
	assert.NoDirExists(t, meta.DataDir)

	_, err := manager.loadMetadata(forkID)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrNotFound)
}

func TestForkInstance_CleansUpOnTargetTransitionError(t *testing.T) {
	manager, _ := setupTestManager(t)
	ctx := context.Background()

	sourceID := "fork-target-transition-source"
	require.NoError(t, manager.ensureDirectories(sourceID))

	now := time.Now()
	meta := &metadata{StoredMetadata: StoredMetadata{
		Id:                sourceID,
		Name:              sourceID,
		Image:             "docker.io/library/nonexistent:latest",
		CreatedAt:         now,
		StoppedAt:         &now,
		HypervisorType:    hypervisor.TypeCloudHypervisor,
		HypervisorVersion: "test",
		SocketPath:        paths.New(manager.paths.DataDir()).InstanceSocket(sourceID, "cloud-hypervisor.sock"),
		DataDir:           paths.New(manager.paths.DataDir()).InstanceDir(sourceID),
		VsockCID:          42,
		VsockSocket:       paths.New(manager.paths.DataDir()).InstanceVsockSocket(sourceID),
	}}
	require.NoError(t, manager.saveMetadata(meta))

	_, err := manager.ForkInstance(ctx, sourceID, ForkInstanceRequest{
		Name:        "fork-target-transition-copy",
		TargetState: StateRunning,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "apply fork target state")

	entries, err := os.ReadDir(manager.paths.GuestsDir())
	require.NoError(t, err)
	require.Len(t, entries, 1)
	assert.Equal(t, sourceID, entries[0].Name())
}

func TestForkInstanceRejectsDuplicateNameForNonNetworkedSource(t *testing.T) {
	manager, _ := setupTestManager(t)
	ctx := context.Background()

	sourceID := "fork-duplicate-name-source"
	require.NoError(t, manager.ensureDirectories(sourceID))

	now := time.Now()
	sourceMeta := &metadata{StoredMetadata: StoredMetadata{
		Id:                sourceID,
		Name:              sourceID,
		Image:             "docker.io/library/alpine:latest",
		CreatedAt:         now,
		StoppedAt:         &now,
		HypervisorType:    hypervisor.TypeCloudHypervisor,
		HypervisorVersion: "test",
		SocketPath:        paths.New(manager.paths.DataDir()).InstanceSocket(sourceID, "cloud-hypervisor.sock"),
		DataDir:           paths.New(manager.paths.DataDir()).InstanceDir(sourceID),
		VsockCID:          42,
		VsockSocket:       paths.New(manager.paths.DataDir()).InstanceVsockSocket(sourceID),
	}}
	require.NoError(t, manager.saveMetadata(sourceMeta))

	existingID := "fork-duplicate-name-existing"
	require.NoError(t, manager.ensureDirectories(existingID))
	existingMeta := &metadata{StoredMetadata: StoredMetadata{
		Id:                existingID,
		Name:              "duplicate-name",
		Image:             "docker.io/library/alpine:latest",
		CreatedAt:         now,
		StoppedAt:         &now,
		HypervisorType:    hypervisor.TypeCloudHypervisor,
		HypervisorVersion: "test",
		SocketPath:        paths.New(manager.paths.DataDir()).InstanceSocket(existingID, "cloud-hypervisor.sock"),
		DataDir:           paths.New(manager.paths.DataDir()).InstanceDir(existingID),
		VsockCID:          43,
		VsockSocket:       paths.New(manager.paths.DataDir()).InstanceVsockSocket(existingID),
	}}
	require.NoError(t, manager.saveMetadata(existingMeta))

	_, err := manager.ForkInstance(ctx, sourceID, ForkInstanceRequest{Name: "duplicate-name"})
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrAlreadyExists)
	assert.Contains(t, err.Error(), "already exists")
}

func TestCloneStoredMetadataForFork_DeepCopiesReferenceFields(t *testing.T) {
	startedAt := time.Now().Add(-2 * time.Minute)
	stoppedAt := time.Now().Add(-1 * time.Minute)
	pid := 1234
	exitCode := 17

	src := StoredMetadata{
		Env:           map[string]string{"A": "1"},
		Metadata:      map[string]string{"m": "x"},
		Volumes:       []VolumeAttachment{{VolumeID: "vol-1", MountPath: "/data"}},
		Devices:       []string{"0000:01:00.0"},
		Entrypoint:    []string{"/bin/sh", "-c"},
		Cmd:           []string{"echo", "hello"},
		StartedAt:     &startedAt,
		StoppedAt:     &stoppedAt,
		HypervisorPID: &pid,
		ExitCode:      &exitCode,
	}

	cloned := cloneStoredMetadataForFork(src)
	require.Equal(t, src, cloned)

	cloned.Env["A"] = "2"
	cloned.Metadata["m"] = "y"
	cloned.Volumes[0].MountPath = "/mnt"
	cloned.Devices[0] = "0000:02:00.0"
	cloned.Entrypoint[0] = "/usr/bin/env"
	cloned.Cmd[0] = "printf"
	*cloned.HypervisorPID = 4321
	*cloned.ExitCode = 42
	now := time.Now()
	*cloned.StartedAt = now
	*cloned.StoppedAt = now

	require.Equal(t, "1", src.Env["A"])
	require.Equal(t, "x", src.Metadata["m"])
	require.Equal(t, "/data", src.Volumes[0].MountPath)
	require.Equal(t, "0000:01:00.0", src.Devices[0])
	require.Equal(t, "/bin/sh", src.Entrypoint[0])
	require.Equal(t, "echo", src.Cmd[0])
	require.Equal(t, 1234, *src.HypervisorPID)
	require.Equal(t, 17, *src.ExitCode)
	require.Equal(t, startedAt, *src.StartedAt)
	require.Equal(t, stoppedAt, *src.StoppedAt)
}

func TestRotateSourceVsockForRestore_CloudHypervisorDoesNotPersistCIDRewrite(t *testing.T) {
	manager, _ := setupTestManager(t)
	ctx := context.Background()

	sourceID := "fork-rotate-ch-source"
	forkID := "fork-rotate-ch-fork"
	require.NoError(t, manager.ensureDirectories(sourceID))

	snapshotConfigPath := manager.paths.InstanceSnapshotConfig(sourceID)
	require.NoError(t, os.MkdirAll(filepath.Dir(snapshotConfigPath), 0755))
	require.NoError(t, os.WriteFile(snapshotConfigPath, []byte(`{"vsock":{"cid":100,"socket":"/tmp/vsock.sock"}}`), 0644))

	meta := &metadata{StoredMetadata: StoredMetadata{
		Id:             sourceID,
		Name:           sourceID,
		CreatedAt:      time.Now(),
		HypervisorType: hypervisor.TypeCloudHypervisor,
		SocketPath:     manager.paths.InstanceSocket(sourceID, "cloud-hypervisor.sock"),
		DataDir:        manager.paths.InstanceDir(sourceID),
		VsockCID:       100,
		VsockSocket:    manager.paths.InstanceVsockSocket(sourceID),
	}}
	require.NoError(t, manager.saveMetadata(meta))

	expectedCID := generateForkSourceVsockCID(sourceID, forkID, meta.StoredMetadata.VsockCID)
	require.NotEqual(t, meta.StoredMetadata.VsockCID, expectedCID)

	require.NoError(t, manager.rotateSourceVsockForRestore(ctx, sourceID, forkID))

	updated, err := manager.loadMetadata(sourceID)
	require.NoError(t, err)
	assert.Equal(t, int64(100), updated.StoredMetadata.VsockCID)
}

func TestRotateSourceVsockForRestore_QEMUPersistsCIDRewrite(t *testing.T) {
	manager, _ := setupTestManager(t)
	ctx := context.Background()

	sourceID := "fork-rotate-qemu-source"
	forkID := "fork-rotate-qemu-fork"
	require.NoError(t, manager.ensureDirectories(sourceID))

	snapshotConfigPath := manager.paths.InstanceSnapshotConfig(sourceID)
	snapshotDir := filepath.Dir(snapshotConfigPath)
	require.NoError(t, os.MkdirAll(snapshotDir, 0755))
	require.NoError(t, os.WriteFile(snapshotConfigPath, []byte(`{}`), 0644))

	qemuConfig, err := json.Marshal(hypervisor.VMConfig{VsockCID: 100, VsockSocket: manager.paths.InstanceVsockSocket(sourceID)})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(snapshotDir, "qemu-config.json"), qemuConfig, 0644))

	meta := &metadata{StoredMetadata: StoredMetadata{
		Id:             sourceID,
		Name:           sourceID,
		CreatedAt:      time.Now(),
		HypervisorType: hypervisor.TypeQEMU,
		SocketPath:     manager.paths.InstanceSocket(sourceID, "qemu.sock"),
		DataDir:        manager.paths.InstanceDir(sourceID),
		VsockCID:       100,
		VsockSocket:    manager.paths.InstanceVsockSocket(sourceID),
	}}
	require.NoError(t, manager.saveMetadata(meta))

	expectedCID := generateForkSourceVsockCID(sourceID, forkID, meta.StoredMetadata.VsockCID)
	require.NoError(t, manager.rotateSourceVsockForRestore(ctx, sourceID, forkID))

	updated, err := manager.loadMetadata(sourceID)
	require.NoError(t, err)
	assert.Equal(t, expectedCID, updated.StoredMetadata.VsockCID)
}

func TestForkCloudHypervisorFromRunningNetwork(t *testing.T) {
	if _, err := os.Stat("/dev/kvm"); os.IsNotExist(err) {
		t.Skip("/dev/kvm not available, skipping on this platform")
	}

	manager, tmpDir := setupTestManager(t)
	ctx := context.Background()

	imageManager, err := images.NewManager(paths.New(tmpDir), 1, nil)
	require.NoError(t, err)

	t.Log("Ensuring nginx image...")
	nginxImage, err := imageManager.CreateImage(ctx, images.CreateImageRequest{Name: "docker.io/library/nginx:alpine"})
	require.NoError(t, err)

	imageName := nginxImage.Name
	for i := 0; i < 60; i++ {
		img, err := imageManager.GetImage(ctx, imageName)
		if err == nil && img.Status == images.StatusReady {
			nginxImage = img
			break
		}
		if err == nil && img.Status == images.StatusFailed {
			t.Fatalf("image build failed: %s", *img.Error)
		}
		time.Sleep(1 * time.Second)
	}
	require.Equal(t, images.StatusReady, nginxImage.Status, "Image should be ready after 60 seconds")

	systemManager := manager.systemManager
	require.NoError(t, systemManager.EnsureSystemFiles(ctx))

	require.NoError(t, manager.networkManager.Initialize(ctx, nil))

	source, err := manager.CreateInstance(ctx, CreateInstanceRequest{
		Name:           "fork-running-src",
		Image:          "docker.io/library/nginx:alpine",
		Size:           2 * 1024 * 1024 * 1024,
		HotplugSize:    256 * 1024 * 1024,
		OverlaySize:    10 * 1024 * 1024 * 1024,
		Vcpus:          1,
		NetworkEnabled: true,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = manager.DeleteInstance(context.Background(), source.Id) })
	require.NoError(t, waitForVMReady(ctx, source.SocketPath, 5*time.Second))
	require.NoError(t, waitForLogMessage(ctx, manager, source.Id, "start worker processes", 15*time.Second))

	assert.NotEmpty(t, source.IP)
	assert.NotEmpty(t, source.MAC)
	assertHostCanReachNginx(t, source.IP, 80, 60*time.Second)

	// Default behavior remains strict: running source requires explicit opt-in.
	_, err = manager.ForkInstance(ctx, source.Id, ForkInstanceRequest{Name: "fork-running-no-flag"})
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrInvalidState)

	// Fork from running (internally: standby source -> copy fork -> restore source).
	forked, err := manager.ForkInstance(ctx, source.Id, ForkInstanceRequest{
		Name:        "fork-running-copy",
		FromRunning: true,
		TargetState: StateRunning,
	})
	require.NoError(t, err)
	require.Equal(t, StateRunning, forked.State)
	forkedID := forked.Id
	t.Cleanup(func() { _ = manager.DeleteInstance(context.Background(), forkedID) })

	// Source should be restored and still reachable by its private IP.
	sourceAfterFork, err := manager.GetInstance(ctx, source.Id)
	require.NoError(t, err)
	require.Equal(t, StateRunning, sourceAfterFork.State)
	require.NotEmpty(t, sourceAfterFork.IP)
	assertHostCanReachNginx(t, sourceAfterFork.IP, 80, 60*time.Second)

	// Fork should already be running with target_state=Running.
	require.NoError(t, waitForVMReady(ctx, forked.SocketPath, 5*time.Second))

	assert.NotEmpty(t, forked.IP)
	assert.NotEmpty(t, forked.MAC)
	assert.NotEqual(t, sourceAfterFork.IP, forked.IP)
	assert.NotEqual(t, sourceAfterFork.MAC, forked.MAC)
	assertGuestHasOnlyExpectedIPv4(t, forked, forked.IP, 30*time.Second)
	assertHostCanReachNginx(t, forked.IP, 80, 60*time.Second)
	assertHostCanReachNginx(t, sourceAfterFork.IP, 80, 60*time.Second)
}

func assertHostCanReachNginx(t *testing.T, ip string, port int, timeout time.Duration) {
	t.Helper()

	deadline := time.Now().Add(timeout)
	url := fmt.Sprintf("http://%s:%d/", ip, port)
	client := &http.Client{Timeout: 3 * time.Second}

	var lastErr error
	for time.Now().Before(deadline) {
		resp, err := client.Get(url)
		if err == nil {
			body, readErr := io.ReadAll(resp.Body)
			resp.Body.Close()
			if readErr == nil && resp.StatusCode == http.StatusOK && strings.Contains(string(body), "Welcome to nginx!") {
				return
			}
			if readErr != nil {
				lastErr = fmt.Errorf("read body: %w", readErr)
			} else {
				lastErr = fmt.Errorf("status=%d body=%q", resp.StatusCode, string(body))
			}
		} else {
			lastErr = err
		}
		time.Sleep(500 * time.Millisecond)
	}

	require.NoError(t, lastErr, "host should reach %s within %s", url, timeout)
}

func assertGuestHasOnlyExpectedIPv4(t *testing.T, inst *Instance, expectedIP string, timeout time.Duration) {
	t.Helper()

	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		output, exitCode, err := execInInstance(context.Background(), inst, "sh", "-c", "ip -4 -o addr show dev eth0 scope global | awk '{print $4}'")
		if err == nil && exitCode == 0 {
			var cidrs []string
			for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
				if trimmed := strings.TrimSpace(line); trimmed != "" {
					cidrs = append(cidrs, trimmed)
				}
			}

			if len(cidrs) == 1 && strings.HasPrefix(cidrs[0], expectedIP+"/") {
				return
			}

			lastErr = fmt.Errorf("expected only %s on eth0, got %v", expectedIP, cidrs)
		} else if err != nil {
			lastErr = err
		} else {
			lastErr = fmt.Errorf("ip addr command exit code %d, output=%q", exitCode, output)
		}

		time.Sleep(500 * time.Millisecond)
	}

	require.NoError(t, lastErr, "guest should expose only the fork IP on eth0 within %s", timeout)
}

func execInInstance(ctx context.Context, inst *Instance, command ...string) (string, int, error) {
	dialer, err := hypervisor.NewVsockDialer(inst.HypervisorType, inst.VsockSocket, inst.VsockCID)
	if err != nil {
		return "", -1, err
	}

	var stdout, stderr bytes.Buffer
	exit, err := guest.ExecIntoInstance(ctx, dialer, guest.ExecOptions{
		Command:      command,
		Stdout:       &stdout,
		Stderr:       &stderr,
		WaitForAgent: 30 * time.Second,
	})
	if err != nil {
		return "", -1, err
	}

	output := stdout.String()
	if stderr.Len() > 0 {
		output += "\nSTDERR: " + stderr.String()
	}
	return output, exit.Code, nil
}
