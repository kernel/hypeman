package images

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Code-Hex/vz/v4/diskimagekit"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	gcr "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
	"github.com/google/go-containerregistry/pkg/v1/types"
	"github.com/kernel/hypeman/lib/paths"
	"github.com/stretchr/testify/require"
)

// macOSFixtureChain writes a small ASIF chain to dir: a base with data at two
// offsets and an overlay that replaces the second. It returns their paths.
func macOSFixtureChain(t *testing.T, dir string) (string, string) {
	t.Helper()
	base, overlay := filepath.Join(dir, "base.asif"), filepath.Join(dir, "overlay.asif")
	img, err := diskimagekit.CreateASIF(base, (64<<20)/512)
	require.NoError(t, err)
	_, err = img.WriteAt([]byte("base data"), 0)
	require.NoError(t, err)
	_, err = img.WriteAt([]byte("replaced in overlay"), 32<<20)
	require.NoError(t, err)
	require.NoError(t, img.CreateLayer(overlay, diskimagekit.Overlay))
	_, err = img.WriteAt([]byte("overlay data"), 32<<20)
	require.NoError(t, err)
	require.NoError(t, img.Close())
	return base, overlay
}

func macOSFixturePlatform(t *testing.T) []byte {
	t.Helper()
	b, err := json.Marshal(MacOSImage{HardwareModel: []byte{1}, MachineIdentifier: []byte{2}, MAC: "02:00:00:00:00:01", CPUs: 4, Memory: 8 << 30})
	require.NoError(t, err)
	return b
}

// macOSFixture writes an unpacked one-layer image: root/0 holds the disk, the
// auxiliary storage and the platform.
func macOSFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	layer := filepath.Join(root, "0")
	require.NoError(t, os.MkdirAll(layer, 0700))
	for f, data := range map[string][]byte{"config.json": macOSFixturePlatform(t), "disk.asif": []byte("disk"), "aux.img": []byte("synthetic aux")} {
		require.NoError(t, os.WriteFile(filepath.Join(layer, f), data, 0600))
	}
	return root
}

func macOSFixtureMetadata() *containerMetadata {
	return &containerMetadata{OS: "darwin", Architecture: "arm64", Labels: map[string]string{
		MacOSMachineVersionLabel: "2", MacOSMachineKindLabel: "macos-image", MacOSMachineFormatLabel: "asif",
		MacOSMachineDiskLabel: "disk.asif", MacOSMachineAuxLabel: "aux.img", MacOSMachinePlatformLabel: "config.json",
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
		{"unsupported version", MacOSMachineVersionLabel, "1"}, {"container not machine", MacOSMachineKindLabel, ""},
		{"raw format", MacOSMachineFormatLabel, "raw"}, {"absolute path", MacOSMachineDiskLabel, "/etc/passwd"},
		{"traversal", MacOSMachineDiskLabel, "../disk.asif"}, {"missing aux", MacOSMachineAuxLabel, "missing.img"},
		{"duplicate file", MacOSMachineAuxLabel, "disk.asif"},
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
	require.NoError(t, os.WriteFile(filepath.Join(root, "0", "config.json"), []byte(`{}`), 0600))
	_, err = parseMacOSMachine(root, macOSFixtureMetadata())
	require.Error(t, err)

	// EUI-64 and 20-octet InfiniBand addresses parse with net.ParseMAC but are not Ethernet.
	for _, mac := range []string{"02:00:00:00:00:00:00:01", strings.Repeat("00:", 19) + "01"} {
		b, err := json.Marshal(MacOSImage{HardwareModel: []byte{1}, MachineIdentifier: []byte{2}, MAC: mac, CPUs: 4, Memory: 8 << 30})
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(filepath.Join(root, "0", "config.json"), b, 0600))
		_, err = parseMacOSMachine(root, macOSFixtureMetadata())
		require.Error(t, err, mac)
	}

}

// machineFixtureLayer writes files into a gzipped tar layer and returns it.
func machineFixtureLayer(t *testing.T, dir, name string, files map[string]string) gcr.Layer {
	t.Helper()
	path := filepath.Join(dir, name+".tar.gz")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
	require.NoError(t, err)
	gz, err := gzip.NewWriterLevel(file, gzip.BestSpeed)
	require.NoError(t, err)
	tw := tar.NewWriter(gz)
	for _, entry := range slices.Sorted(maps.Keys(files)) {
		input, err := os.Open(files[entry])
		require.NoError(t, err)
		info, err := input.Stat()
		require.NoError(t, err)
		// The registry controls modes; a permissive file must not become the canonical image mode.
		require.NoError(t, tw.WriteHeader(&tar.Header{Name: entry, Mode: 0666, Size: info.Size(), Typeflag: tar.TypeReg}))
		_, err = io.Copy(tw, input)
		require.NoError(t, err)
		require.NoError(t, input.Close())
	}
	require.NoError(t, tw.Close())
	require.NoError(t, gz.Close())
	require.NoError(t, file.Close())
	layer, err := tarball.LayerFromFile(path)
	require.NoError(t, err)
	return layer
}

// TestMacOSMachineOCIRoundTrip pushes a two-layer machine image, a base disk and an
// overlay with the auxiliary storage and platform, and pulls it through the image
// manager. It is not a VZ boot.
func TestMacOSMachineOCIRoundTrip(t *testing.T) {
	allowMacOSMachines(t)
	work := t.TempDir()
	base, overlay := macOSFixtureChain(t, work)
	aux, config := filepath.Join(work, "aux.img"), filepath.Join(work, "config.json")
	require.NoError(t, os.WriteFile(aux, []byte("synthetic aux"), 0600))
	require.NoError(t, os.WriteFile(config, macOSFixturePlatform(t), 0600))
	image, err := mutate.AppendLayers(empty.Image,
		machineFixtureLayer(t, work, "base", map[string]string{"disk.asif": base}),
		machineFixtureLayer(t, work, "top", map[string]string{"disk.asif": overlay, "aux.img": aux, "config.json": config}))
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
	require.Equal(t, int64(64<<20), *pulled.SizeBytes, "the recorded size is the chain's logical size")

	chain, err := MacOSDiskChain(p, pulled.Name, pulled.Digest)
	require.NoError(t, err)
	require.Len(t, chain, 2)
	for i, want := range []string{base, overlay} {
		got, err := fileSHA256(ctx, chain[i])
		require.NoError(t, err)
		expected, err := fileSHA256(ctx, want)
		require.NoError(t, err)
		require.Equal(t, expected, got, "layer %d is installed byte for byte", i)
		info, err := os.Stat(chain[i])
		require.NoError(t, err)
		require.Equal(t, os.FileMode(0600), info.Mode().Perm())
	}
	stacked, err := OpenMacOSDiskChain(chain)
	require.NoError(t, err)
	for offset, want := range map[int64]string{0: "base data", 32 << 20: "overlay data"} {
		buf := make([]byte, len(want))
		_, err := stacked.ReadAt(buf, offset)
		require.NoError(t, err)
		require.Equal(t, want, string(buf))
	}
	require.NoError(t, stacked.Close())
	installedAux, err := os.ReadFile(filepath.Join(filepath.Dir(chain[0]), "aux.img"))
	require.NoError(t, err)
	require.Equal(t, "synthetic aux", string(installedAux))
	var platform MacOSImage
	require.NoError(t, json.Unmarshal(macOSFixturePlatform(t), &platform))
	require.Equal(t, platform, *pulled.MacOS)

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
	tagChain, err := MacOSDiskChain(p, tagged.Name, tagged.Digest)
	require.NoError(t, err)
	require.Equal(t, chain, tagChain)
	_, err = manager.TagImage(ctx, ref.Name(), strings.TrimPrefix(server.URL, "http://")+"/other:stable")
	require.ErrorIs(t, err, ErrInvalidPlatform)
	_, err = manager.CreateImage(ctx, CreateImageRequest{Name: ref.Name(), Platform: "linux/arm64"})
	require.ErrorIs(t, err, ErrInvalidPlatform)
}

// TestMacOSMachinePullRejectsBrokenChain pulls an image whose overlay layer does not
// belong to its base, and expects the pull to fail.
func TestMacOSMachinePullRejectsBrokenChain(t *testing.T) {
	allowMacOSMachines(t)
	work := t.TempDir()
	_, overlay := macOSFixtureChain(t, work)
	otherBase, _ := macOSFixtureChain(t, t.TempDir())
	aux, config := filepath.Join(work, "aux.img"), filepath.Join(work, "config.json")
	require.NoError(t, os.WriteFile(aux, []byte("synthetic aux"), 0600))
	require.NoError(t, os.WriteFile(config, macOSFixturePlatform(t), 0600))
	image, err := mutate.AppendLayers(empty.Image,
		machineFixtureLayer(t, work, "base", map[string]string{"disk.asif": otherBase}),
		machineFixtureLayer(t, work, "top", map[string]string{"disk.asif": overlay, "aux.img": aux, "config.json": config}))
	require.NoError(t, err)
	cfg, err := image.ConfigFile()
	require.NoError(t, err)
	cfg.OS, cfg.Architecture, cfg.Config.Labels = "darwin", "arm64", macOSFixtureMetadata().Labels
	image, err = mutate.ConfigFile(image, cfg)
	require.NoError(t, err)
	server := httptest.NewServer(registry.New())
	defer server.Close()
	ref, err := name.ParseReference(strings.TrimPrefix(server.URL, "http://") + "/macos:broken")
	require.NoError(t, err)
	require.NoError(t, remote.Write(ref, image))
	manager, err := NewManager(paths.New(filepath.Join(work, "hypeman-data")), 1, nil)
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	pulled, err := manager.CreateImage(ctx, CreateImageRequest{Name: ref.Name(), Platform: "darwin/arm64"})
	require.NoError(t, err)
	require.Error(t, manager.WaitForReady(ctx, pulled.Name))
	pulled, err = manager.GetImage(ctx, pulled.Name)
	require.NoError(t, err)
	require.Equal(t, StatusFailed, pulled.Status)
}

func TestValidateMacOSMachineRejectsBeforeUnpack(t *testing.T) {
	allowMacOSMachines(t)
	valid := map[string]string{MacOSMachineVersionLabel: "2", MacOSMachineKindLabel: "macos-image", MacOSMachineFormatLabel: "asif",
		MacOSMachineDiskLabel: "disk.asif", MacOSMachineAuxLabel: "aux.img", MacOSMachinePlatformLabel: "platform.json"}
	require.False(t, isMacOSMachine(&containerMetadata{OS: "linux", Architecture: "amd64"}))
	require.True(t, isMacOSMachine(&containerMetadata{OS: "Darwin", Architecture: "aarch64"}))
	require.NoError(t, validateMacOSMachine(&containerMetadata{OS: "darwin", Architecture: "arm64", Labels: valid}))
	require.Error(t, validateMacOSMachine(&containerMetadata{OS: "darwin", Architecture: "arm64"}), "darwin artifact without machine labels must fail before layer unpack")
	require.ErrorContains(t, validateMacOSMachine(&containerMetadata{OS: "darwin", Architecture: "amd64", Labels: valid}), "darwin/amd64")

	for name, change := range map[string]func(map[string]string){
		"missing disk path":     func(l map[string]string) { delete(l, MacOSMachineDiskLabel) },
		"missing aux path":      func(l map[string]string) { delete(l, MacOSMachineAuxLabel) },
		"missing platform path": func(l map[string]string) { delete(l, MacOSMachinePlatformLabel) },
		"absolute path":         func(l map[string]string) { l[MacOSMachineDiskLabel] = "/disk.asif" },
		"escaping path":         func(l map[string]string) { l[MacOSMachineAuxLabel] = "../aux.img" },
		"duplicate path":        func(l map[string]string) { l[MacOSMachineAuxLabel] = "./disk.asif" },
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
