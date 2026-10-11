package images

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
	"github.com/google/go-containerregistry/pkg/v1/types"
	"github.com/kernel/hypeman/lib/paths"
	"github.com/stretchr/testify/require"
)

func macOSFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	c := MacOSImage{HardwareModel: []byte{1}, MachineIdentifier: []byte{2}, MAC: "02:00:00:00:00:01", CPUs: 4, Memory: 8 << 30}
	b, err := json.Marshal(c)
	require.NoError(t, err)
	// A mostly empty disk, like a real one, so transport can be checked for sparseness.
	disk := make([]byte, 64<<20)
	copy(disk, "not a real boot disk: synthetic OCI transport fixture")
	copy(disk[len(disk)-4:], "tail")
	for f, data := range map[string][]byte{"config.json": b, "disk.img": disk, "aux.img": []byte("synthetic aux")} {
		require.NoError(t, os.WriteFile(filepath.Join(root, f), data, 0600))
	}
	return root
}

func macOSFixtureMetadata() *containerMetadata {
	return &containerMetadata{OS: "darwin", Architecture: "arm64", Labels: map[string]string{
		MacOSMachineVersionLabel: "1", MacOSMachineKindLabel: "macos-image", MacOSMachineFormatLabel: "raw",
		MacOSMachineDiskLabel: "disk.img", MacOSMachineAuxLabel: "aux.img", MacOSMachinePlatformLabel: "config.json",
	}}
}

// parseMacOSMachine validates labels and reads the bundle, as a pull does before
// and after unpacking.
func parseMacOSMachine(root string, meta *containerMetadata) (*macOSMachinePayload, error) {
	if err := validateMacOSMachine(meta); err != nil {
		return nil, err
	}
	return readMacOSMachine(root, meta)
}

func TestMacOSMachineValidation(t *testing.T) {
	allowMacOSMachines(t)
	root := macOSFixture(t)
	payload, err := parseMacOSMachine(root, macOSFixtureMetadata())
	require.NoError(t, err)
	require.NotNil(t, payload)
	for _, tc := range []struct{ name, key, value string }{
		{"unsupported version", MacOSMachineVersionLabel, "2"}, {"container not machine", MacOSMachineKindLabel, ""},
		{"wrong format", MacOSMachineFormatLabel, "qcow2"}, {"absolute path", MacOSMachineDiskLabel, "/etc/passwd"},
		{"traversal", MacOSMachineDiskLabel, "../disk.img"}, {"missing aux", MacOSMachineAuxLabel, "missing.img"},
		{"duplicate file", MacOSMachineAuxLabel, "disk.img"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			meta := macOSFixtureMetadata()
			meta.Labels[tc.key] = tc.value
			_, err := parseMacOSMachine(root, meta)
			require.Error(t, err)
		})
	}
	// Platform aliases normalize before the machine check, as they do for manifest matching.
	for _, alias := range []struct{ os, arch string }{{"darwin", "aarch64"}, {"Darwin", "arm64"}} {
		meta := macOSFixtureMetadata()
		meta.OS, meta.Architecture = alias.os, alias.arch
		payload, err := parseMacOSMachine(root, meta)
		require.NoError(t, err, alias)
		require.NotNil(t, payload, alias)
	}
	require.NoError(t, os.WriteFile(filepath.Join(root, "config.json"), []byte(`{}`), 0600))
	_, err = parseMacOSMachine(root, macOSFixtureMetadata())
	require.Error(t, err)

	// EUI-64 and 20-octet InfiniBand addresses parse with net.ParseMAC but are not Ethernet.
	for _, mac := range []string{"02:00:00:00:00:00:00:01", strings.Repeat("00:", 19) + "01"} {
		b, err := json.Marshal(MacOSImage{HardwareModel: []byte{1}, MachineIdentifier: []byte{2}, MAC: mac, CPUs: 4, Memory: 8 << 30})
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(filepath.Join(root, "config.json"), b, 0600))
		_, err = parseMacOSMachine(root, macOSFixtureMetadata())
		require.Error(t, err, mac)
	}

}

// TestMacOSMachineOCIRoundTrip pushes a machine bundle to a registry and pulls it
// through the image manager. It is not a VZ boot.
func TestMacOSMachineOCIRoundTrip(t *testing.T) {
	allowMacOSMachines(t)
	source := macOSFixture(t)
	work := t.TempDir()
	layerPath := filepath.Join(work, "bundle.tar.gz")
	file, err := os.OpenFile(layerPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
	require.NoError(t, err)
	gz, err := gzip.NewWriterLevel(file, gzip.BestSpeed)
	require.NoError(t, err)
	tw := tar.NewWriter(gz)
	hashes := map[string]string{}
	for _, f := range []string{"disk.img", "aux.img", "config.json"} {
		input, err := os.Open(filepath.Join(source, f))
		require.NoError(t, err)
		info, err := input.Stat()
		require.NoError(t, err)
		// The registry controls modes; a permissive disk must not become the canonical image mode.
		mode := int64(0600)
		if f == "disk.img" {
			mode = 0666
		}
		require.NoError(t, tw.WriteHeader(&tar.Header{Name: f, Mode: mode, Size: info.Size(), Typeflag: tar.TypeReg}))
		h := sha256.New()
		_, err = io.Copy(io.MultiWriter(tw, h), input)
		require.NoError(t, err)
		require.NoError(t, input.Close())
		hashes[f] = fmt.Sprintf("%x", h.Sum(nil))
	}
	require.NoError(t, tw.Close())
	require.NoError(t, gz.Close())
	require.NoError(t, file.Close())
	layer, err := tarball.LayerFromFile(layerPath)
	require.NoError(t, err)
	image, err := mutate.AppendLayers(empty.Image, layer)
	require.NoError(t, err)
	cfg, err := image.ConfigFile()
	require.NoError(t, err)
	cfg.OS = "darwin"
	cfg.Architecture = "arm64"
	cfg.Config.Labels = macOSFixtureMetadata().Labels
	image, err = mutate.ConfigFile(image, cfg)
	require.NoError(t, err)
	server := httptest.NewServer(registry.New())
	defer server.Close()
	ref, err := name.ParseReference(strings.TrimPrefix(server.URL, "http://") + "/macos:spike")
	require.NoError(t, err)
	require.NoError(t, remote.Write(ref, image))
	p := paths.New(filepath.Join(work, "hypeman-data"))
	manager, err := NewManager(p, 1, nil)
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	pulled, err := manager.CreateImage(ctx, CreateImageRequest{Name: ref.Name(), Platform: "darwin/arm64"})
	require.NoError(t, err)
	require.NoError(t, manager.WaitForReady(ctx, pulled.Name))
	pulled, err = manager.GetImage(ctx, pulled.Name)
	require.NoError(t, err)
	require.Equal(t, StatusReady, pulled.Status)
	require.Equal(t, "darwin/arm64", pulled.Platform)
	require.NotNil(t, pulled.MacOS)
	disk, err := GetDiskPath(p, pulled.Name, pulled.Digest)
	require.NoError(t, err)
	require.Equal(t, machineDiskFile, filepath.Base(disk), "a raw machine disk is not named as a filesystem")
	for f, path := range map[string]string{"disk.img": disk, "aux.img": filepath.Join(filepath.Dir(disk), "aux.img")} {
		sum, err := fileSHA256(ctx, path)
		require.NoError(t, err)
		require.Equal(t, "sha256:"+hashes[f], sum, f)
		info, err := os.Stat(path)
		require.NoError(t, err)
		require.Equal(t, os.FileMode(0600), info.Mode().Perm(), f)
		if f == "disk.img" {
			control := sparseControlBytes(t, filepath.Dir(path), info.Size())
			require.LessOrEqual(t, allocatedBytes(t, path), control+2*machineUnpackChunk, "the pulled disk stays sparse")
		}
	}
	expected, err := os.ReadFile(filepath.Join(source, "config.json"))
	require.NoError(t, err)
	var config MacOSImage
	require.NoError(t, json.Unmarshal(expected, &config))
	require.Equal(t, config, *pulled.MacOS)
	digest, err := image.Digest()
	require.NoError(t, err)
	require.Equal(t, digest.String(), pulled.Digest)
	reused, err := manager.CreateImage(ctx, CreateImageRequest{Name: ref.Name(), Platform: "darwin/arm64"})
	require.NoError(t, err)
	require.Equal(t, StatusReady, reused.Status)
	tagged, err := manager.TagImage(ctx, ref.Name(), ref.Context().Name()+":stable")
	require.NoError(t, err)
	require.Equal(t, pulled.Digest, tagged.Digest)
	require.Equal(t, pulled.MacOS, tagged.MacOS)
	tagDisk, err := GetDiskPath(p, tagged.Name, tagged.Digest)
	require.NoError(t, err)
	require.Equal(t, disk, tagDisk)
	_, err = manager.TagImage(ctx, ref.Name(), strings.TrimPrefix(server.URL, "http://")+"/other:stable")
	require.ErrorIs(t, err, ErrInvalidPlatform)
	_, err = manager.CreateImage(ctx, CreateImageRequest{Name: ref.Name(), Platform: "linux/arm64"})
	require.ErrorIs(t, err, ErrInvalidPlatform)
	t.Log("round-trip verified", pulled.Digest)
}

func TestValidateMacOSMachineRejectsBeforeUnpack(t *testing.T) {
	allowMacOSMachines(t)
	valid := map[string]string{MacOSMachineVersionLabel: "1", MacOSMachineKindLabel: "macos-image", MacOSMachineFormatLabel: "raw",
		MacOSMachineDiskLabel: "disk.img", MacOSMachineAuxLabel: "aux.img", MacOSMachinePlatformLabel: "platform.json"}
	require.False(t, isMacOSMachine(&containerMetadata{OS: "linux", Architecture: "amd64"}))
	require.True(t, isMacOSMachine(&containerMetadata{OS: "Darwin", Architecture: "aarch64"}))
	require.NoError(t, validateMacOSMachine(&containerMetadata{OS: "darwin", Architecture: "arm64", Labels: valid}))
	require.Error(t, validateMacOSMachine(&containerMetadata{OS: "darwin", Architecture: "arm64"}), "darwin artifact without machine labels must fail before layer unpack")
	require.ErrorContains(t, validateMacOSMachine(&containerMetadata{OS: "darwin", Architecture: "amd64", Labels: valid}), "darwin/amd64")

	for name, change := range map[string]func(map[string]string){
		"missing disk path":     func(l map[string]string) { delete(l, MacOSMachineDiskLabel) },
		"missing aux path":      func(l map[string]string) { delete(l, MacOSMachineAuxLabel) },
		"missing platform path": func(l map[string]string) { delete(l, MacOSMachinePlatformLabel) },
		"absolute path":         func(l map[string]string) { l[MacOSMachineDiskLabel] = "/disk.img" },
		"escaping path":         func(l map[string]string) { l[MacOSMachineAuxLabel] = "../aux.img" },
		"duplicate path":        func(l map[string]string) { l[MacOSMachineAuxLabel] = "./disk.img" },
	} {
		labels := maps.Clone(valid)
		change(labels)
		require.Error(t, validateMacOSMachine(&containerMetadata{OS: "darwin", Architecture: "arm64", Labels: labels}), name)
	}
}

func allowMacOSMachines(t *testing.T) {
	prev := macOSMachineHost
	macOSMachineHost = true
	t.Cleanup(func() { macOSMachineHost = prev })
}

func TestMacOSMachinePullRefusedOnOtherHosts(t *testing.T) {
	prev := macOSMachineHost
	macOSMachineHost = false
	t.Cleanup(func() { macOSMachineHost = prev })

	require.ErrorIs(t, checkMachinePlatformHost(Platform{OS: "darwin", Architecture: "arm64"}), ErrInvalidPlatform)
	require.NoError(t, checkMachinePlatformHost(Platform{OS: "linux", Architecture: "arm64"}))
	require.ErrorIs(t, validateMacOSMachine(&containerMetadata{OS: "darwin", Architecture: "arm64"}), ErrInvalidPlatform, "a darwin manifest pulled without a platform is refused too")
}

// A host that cannot boot macOS refuses a darwin image without downloading its
// layers, including when the request names no platform.
func TestMacOSMachineRefusedBeforeLayerDownloadOnOtherHosts(t *testing.T) {
	prev := macOSMachineHost
	macOSMachineHost = false
	t.Cleanup(func() { macOSMachineHost = prev })

	layer, err := random.Layer(1<<10, types.DockerLayer)
	require.NoError(t, err)
	image, err := mutate.AppendLayers(empty.Image, layer)
	require.NoError(t, err)
	cfg, err := image.ConfigFile()
	require.NoError(t, err)
	cfg.OS, cfg.Architecture, cfg.Config.Labels = "darwin", "arm64", macOSFixtureMetadata().Labels
	image, err = mutate.ConfigFile(image, cfg)
	require.NoError(t, err)
	layerDigest, err := layer.Digest()
	require.NoError(t, err)

	var layerFetches atomic.Int32
	reg := registry.New()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/blobs/"+layerDigest.String()) {
			layerFetches.Add(1)
		}
		reg.ServeHTTP(w, r)
	}))
	defer server.Close()
	ref, err := name.ParseReference(strings.TrimPrefix(server.URL, "http://") + "/macos:spike")
	require.NoError(t, err)
	require.NoError(t, remote.Write(ref, image))
	digest, err := image.Digest()
	require.NoError(t, err)

	manager, err := NewManager(paths.New(t.TempDir()), 1, nil)
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	_, err = manager.CreateImage(ctx, CreateImageRequest{Name: ref.Context().Name() + "@" + digest.String()})
	require.ErrorIs(t, err, ErrInvalidPlatform, "a digest names its platform, so the request is refused at once")

	pending, err := manager.CreateImage(ctx, CreateImageRequest{Name: ref.Name()})
	require.NoError(t, err, "a tag's platform is only known once the worker reads the config")
	err = manager.WaitForReady(ctx, pending.Name)
	require.Error(t, err)
	require.Contains(t, err.Error(), "macOS host")
	require.Zero(t, layerFetches.Load(), "the layer was downloaded before the refusal")
}

// An in-flight darwin pull knows only the platform it was requested as. An explicit
// request for another platform must not be handed that pull.
func TestMacOSMachineInFlightPullRefusesOtherPlatform(t *testing.T) {
	allowMacOSMachines(t)
	layer, err := random.Layer(1<<10, types.DockerLayer)
	require.NoError(t, err)
	image, err := mutate.AppendLayers(empty.Image, layer)
	require.NoError(t, err)
	cfg, err := image.ConfigFile()
	require.NoError(t, err)
	cfg.OS, cfg.Architecture, cfg.Config.Labels = "darwin", "arm64", macOSFixtureMetadata().Labels
	image, err = mutate.ConfigFile(image, cfg)
	require.NoError(t, err)
	server := httptest.NewServer(registry.New())
	defer server.Close()
	ref, err := name.ParseReference(strings.TrimPrefix(server.URL, "http://") + "/macos:spike")
	require.NoError(t, err)
	require.NoError(t, remote.Write(ref, image))
	digest, err := image.Digest()
	require.NoError(t, err)

	p := paths.New(t.TempDir())
	manager, err := NewManager(p, 1, nil)
	require.NoError(t, err)
	normalized, err := ParseNormalizedRef(ref.Name())
	require.NoError(t, err)
	// A darwin pull of this digest is still downloading.
	require.NoError(t, writeMetadata(p, normalized.Repository(), digest.Hex, &imageMetadata{
		Name: ref.Name(), Digest: digest.String(), Status: StatusPulling, BuildID: "in-flight",
		Request: &CreateImageRequest{Name: ref.Name(), Platform: "darwin/arm64"},
	}))

	ctx := context.Background()
	_, err = manager.CreateImage(ctx, CreateImageRequest{Name: ref.Name(), Platform: "linux/arm64"})
	require.ErrorIs(t, err, ErrInvalidPlatform)
	pending, err := manager.CreateImage(ctx, CreateImageRequest{Name: ref.Name(), Platform: "darwin/arm64"})
	require.NoError(t, err, "a matching request still joins the in-flight pull")
	require.Equal(t, StatusPulling, pending.Status)
}
