package images

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	gcrregistry "github.com/google/go-containerregistry/pkg/registry"
	gcr "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
	"github.com/kernel/hypeman/lib/paths"
	"github.com/stretchr/testify/require"
)

func machineArtifactOCIImage(t *testing.T, diskName string, disk []byte, labels map[string]string) gcr.Image {
	t.Helper()
	var layerData bytes.Buffer
	gz := gzip.NewWriter(&layerData)
	tw := tar.NewWriter(gz)
	require.NoError(t, tw.WriteHeader(&tar.Header{Name: diskName, Mode: 0644, Size: int64(len(disk))}))
	_, err := tw.Write(disk)
	require.NoError(t, err)
	require.NoError(t, tw.Close())
	require.NoError(t, gz.Close())

	layer, err := tarball.LayerFromOpener(func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(layerData.Bytes())), nil
	})
	require.NoError(t, err)
	image, err := mutate.AppendLayers(empty.Image, layer)
	require.NoError(t, err)
	config, err := image.ConfigFile()
	require.NoError(t, err)
	config.OS = "windows"
	config.Architecture = "amd64"
	config.Config.Labels = labels
	image, err = mutate.ConfigFile(image, config)
	require.NoError(t, err)
	return image
}

func waitForMachineImage(t *testing.T, manager Manager, name string) *Image {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	require.NoError(t, manager.WaitForReady(ctx, name))
	image, err := manager.GetImage(ctx, name)
	require.NoError(t, err)
	require.Equal(t, StatusReady, image.Status)
	return image
}

func TestMachineArtifactsPullFromOCI(t *testing.T) {
	if _, err := exec.LookPath("qemu-img"); err != nil {
		if os.Getenv("CI") == "true" {
			t.Fatal("qemu-img is required in CI")
		}
		t.Skip("qemu-img is unavailable")
	}

	registry := httptest.NewServer(gcrregistry.New())
	defer registry.Close()
	manager, err := NewManager(paths.New(t.TempDir()), 1, nil)
	require.NoError(t, err)

	baseFile := filepath.Join(t.TempDir(), "base.raw")
	file, err := os.Create(baseFile)
	require.NoError(t, err)
	require.NoError(t, file.Truncate(4<<20))
	require.NoError(t, file.Close())
	baseBytes, err := os.ReadFile(baseFile)
	require.NoError(t, err)
	baseLabels := windowsMachineMetadata(MachineImageWindowsBase, "hypeman/base.raw", "").Labels
	baseImage := machineArtifactOCIImage(t, "hypeman/base.raw", baseBytes, baseLabels)
	baseTag, err := name.NewTag(registry.Listener.Addr().String()+"/windows/base:test", name.Insecure)
	require.NoError(t, err)
	require.NoError(t, remote.Write(baseTag, baseImage))

	createdBase, err := manager.CreateImage(context.Background(), CreateImageRequest{Name: baseTag.String(), Platform: "windows/amd64"})
	require.NoError(t, err)
	readyBase := waitForMachineImage(t, manager, createdBase.Name)
	require.Equal(t, MachineImageWindowsBase, readyBase.Machine.Kind)

	personaFile := filepath.Join(t.TempDir(), "persona.qcow2")
	output, err := exec.Command("qemu-img", "create", "-f", "qcow2", personaFile, "4M").CombinedOutput()
	require.NoError(t, err, "%s", output)
	personaBytes, err := os.ReadFile(personaFile)
	require.NoError(t, err)
	baseReference := baseTag.Context().Name() + "@" + readyBase.Digest
	personaLabels := windowsMachineMetadata(MachineImageWindowsPersona, "hypeman/persona.qcow2", baseReference).Labels
	personaImage := machineArtifactOCIImage(t, "hypeman/persona.qcow2", personaBytes, personaLabels)
	personaTag, err := name.NewTag(registry.Listener.Addr().String()+"/windows/persona:test", name.Insecure)
	require.NoError(t, err)
	require.NoError(t, remote.Write(personaTag, personaImage))

	createdPersona, err := manager.CreateImage(context.Background(), CreateImageRequest{Name: personaTag.String(), Platform: "windows/amd64"})
	require.NoError(t, err)
	readyPersona := waitForMachineImage(t, manager, createdPersona.Name)
	require.Equal(t, MachineImageWindowsPersona, readyPersona.Machine.Kind)
	require.Equal(t, readyBase.Machine.VirtualSize, readyPersona.Machine.VirtualSize)
}
