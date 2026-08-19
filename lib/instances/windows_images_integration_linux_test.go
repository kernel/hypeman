//go:build linux && amd64

package instances

import (
	"context"
	"crypto/sha256"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/kernel/hypeman/lib/forkvm"
	"github.com/kernel/hypeman/lib/hypervisor"
	"github.com/kernel/hypeman/lib/images"
	"github.com/kernel/hypeman/lib/paths"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type windowsFixtureImageManager struct {
	images.Manager
	image *images.Image
}

func (m windowsFixtureImageManager) CreateImage(context.Context, images.CreateImageRequest) (*images.Image, error) {
	copy := *m.image
	return &copy, nil
}

func (m windowsFixtureImageManager) GetImage(context.Context, string) (*images.Image, error) {
	copy := *m.image
	return &copy, nil
}

func (m windowsFixtureImageManager) WaitForReady(context.Context, string) error { return nil }

func requireWindowsFixture(t *testing.T) string {
	t.Helper()
	path := os.Getenv("HYPEMAN_WINDOWS_TEST_PERSONA")
	if path == "" {
		path = "/ci/windows/persona.qcow2"
	}
	if _, err := os.Stat(path); err == nil {
		return path
	}
	if os.Getenv("CI") == "true" {
		t.Fatalf("required Windows persona fixture is missing: %s", path)
	}
	t.Skipf("Windows persona fixture is unavailable: %s", path)
	return ""
}

func TestWindowsImagesIntegration(t *testing.T) {
	fixture := requireWindowsFixture(t)
	acquireHeavyIO(t)

	manager, dataDir := setupTestManagerForQEMU(t)
	p := paths.New(dataDir)
	const digestHex = "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
	image := &images.Image{
		Name:     "registry.example/windows/persona:integration",
		Digest:   "sha256:" + digestHex,
		Platform: "windows/amd64",
		Status:   images.StatusReady,
		Machine: &images.MachineImage{
			Kind:        images.MachineImageWindowsPersona,
			Base:        "registry.example/windows/base@sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd",
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
	personaBytes, err := os.ReadFile(personaPath)
	require.NoError(t, err)
	personaHash := sha256.Sum256(personaBytes)
	personaInfo, err := os.Stat(personaPath)
	require.NoError(t, err)

	ctx := context.Background()
	instance, err := manager.CreateInstance(ctx, CreateInstanceRequest{
		Name:       "windows-images-integration",
		Image:      image.Name,
		Platform:   "windows/amd64",
		Size:       8 << 30,
		Vcpus:      4,
		Hypervisor: hypervisor.TypeQEMU,
	})
	require.NoError(t, err)
	instanceID := instance.Id
	t.Cleanup(func() {
		if instanceID != "" {
			_ = deleteTestInstanceNow(context.Background(), manager, instanceID)
		}
	})
	require.Equal(t, StateInitializing, instance.State)
	require.FileExists(t, p.InstanceWindowsDisk(instance.Id))
	require.FileExists(t, p.InstanceOVMFVars(instance.Id))
	require.DirExists(t, p.InstanceTPMDir(instance.Id))
	assert.NoFileExists(t, p.InstanceOverlay(instance.Id))
	assert.NoFileExists(t, p.InstanceConfigDisk(instance.Id))

	require.Eventually(t, func() bool {
		info, err := os.Stat(p.InstanceWindowsDisk(instance.Id))
		return err == nil && info.Size() > personaInfo.Size()
	}, 60*time.Second, 500*time.Millisecond, "Windows boot must write to the instance qcow2")

	sourceAfter, err := os.ReadFile(personaPath)
	require.NoError(t, err)
	assert.Equal(t, personaHash, sha256.Sum256(sourceAfter), "immutable persona changed during guest boot")

	stopped, err := manager.StopInstance(ctx, instance.Id)
	require.NoError(t, err)
	require.Equal(t, StateStopped, stopped.State)
	output, err := exec.Command("qemu-img", "check", p.InstanceWindowsDisk(instance.Id)).CombinedOutput()
	require.NoError(t, err, "%s", output)

	require.NoError(t, manager.DeleteInstance(ctx, instance.Id))
	instanceID = ""
	assert.NoDirExists(t, filepath.Dir(p.InstanceWindowsDisk(instance.Id)))
}
