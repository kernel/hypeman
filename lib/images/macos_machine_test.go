package images

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
	"github.com/kernel/hypeman/lib/paths"
	"github.com/stretchr/testify/require"
)

func macOSFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	c := MacOSImage{HardwareModel: []byte{1}, MachineIdentifier: []byte{2}, MAC: "02:00:00:00:00:01", CPUs: 4, Memory: 8 << 30}
	b, err := json.Marshal(c)
	require.NoError(t, err)
	for f, data := range map[string][]byte{"config.json": b, "disk.img": []byte("not a real boot disk: synthetic OCI transport fixture"), "aux.img": []byte("synthetic aux")} {
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

func TestMacOSMachineValidation(t *testing.T) {
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
	outside := filepath.Join(t.TempDir(), "outside")
	require.NoError(t, os.WriteFile(outside, []byte("outside"), 0600))
	require.NoError(t, os.Symlink(outside, filepath.Join(root, "escape")))
	meta := macOSFixtureMetadata()
	meta.Labels[MacOSMachineDiskLabel] = "escape"
	_, err = parseMacOSMachine(root, meta)
	require.Error(t, err)
	meta = macOSFixtureMetadata()
	meta.Architecture = "amd64"
	_, err = parseMacOSMachine(root, meta)
	require.Error(t, err)

	// Platform aliases normalize before the machine check, as they do for manifest matching.
	for _, alias := range []struct{ os, arch string }{{"darwin", "aarch64"}, {"Darwin", "arm64"}} {
		meta = macOSFixtureMetadata()
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

	// A hardlink with a different name is the same file and must not satisfy distinctness.
	linked := macOSFixture(t)
	require.NoError(t, os.Link(filepath.Join(linked, "disk.img"), filepath.Join(linked, "alias.img")))
	meta = macOSFixtureMetadata()
	meta.Labels[MacOSMachineAuxLabel] = "alias.img"
	_, err = parseMacOSMachine(linked, meta)
	require.ErrorContains(t, err, "distinct")
}

// Optional real-bundle run only reads a stopped source, never the live benchmark disk.
// It proves normal manager pull/materialization, not VZ boot or API server deployment.
func TestMacOSMachineOCIRoundTrip(t *testing.T) {
	source := macOSFixture(t)
	work := t.TempDir()
	_, err := parseMacOSMachine(source, macOSFixtureMetadata())
	require.NoError(t, err)
	layerPath := filepath.Join(work, "bundle.tar.gz")
	file, err := os.OpenFile(layerPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
	require.NoError(t, err)
	gz, err := gzip.NewWriterLevel(file, gzip.BestSpeed)
	require.NoError(t, err)
	tw := tar.NewWriter(gz)
	hashes := map[string]string{}
	t.Log("packing stopped bundle", source)
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
	t.Log("packed; streaming blobs into loopback registry storage")
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
	require.NoError(t, os.MkdirAll(filepath.Join(work, "registry-blobs"), 0700))
	// The test registry buffers PATCH uploads in memory even with a disk blob
	// handler. Seed blobs through the streaming handler so real VM layers do
	// not consume tens of GiB of RAM; remote.Write still publishes the manifest.
	blobs := registry.NewDiskBlobHandler(filepath.Join(work, "registry-blobs"))
	writer := blobs.(registry.BlobPutHandler)
	layerDigest, err := layer.Digest()
	require.NoError(t, err)
	compressed, err := layer.Compressed()
	require.NoError(t, err)
	err = writer.Put(context.Background(), "macos", layerDigest, compressed)
	closeErr := compressed.Close()
	require.NoError(t, err)
	require.NoError(t, closeErr)
	configDigest, err := image.ConfigName()
	require.NoError(t, err)
	rawConfig, err := image.RawConfigFile()
	require.NoError(t, err)
	require.NoError(t, writer.Put(context.Background(), "macos", configDigest, io.NopCloser(bytes.NewReader(rawConfig))))
	server := httptest.NewServer(registry.New(registry.WithBlobHandler(blobs)))
	defer server.Close()
	ref, err := name.ParseReference(strings.TrimPrefix(server.URL, "http://") + "/macos:spike")
	require.NoError(t, err)
	require.NoError(t, remote.Write(ref, image))
	t.Log("published; pulling through normal image manager")
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
	for f, path := range map[string]string{"disk.img": disk, "aux.img": filepath.Join(filepath.Dir(disk), "aux.img")} {
		input, err := os.Open(path)
		require.NoError(t, err)
		h := sha256.New()
		_, err = io.Copy(h, input)
		require.NoError(t, err)
		require.NoError(t, input.Close())
		require.Equal(t, hashes[f], fmt.Sprintf("%x", h.Sum(nil)), f)
		info, err := os.Stat(path)
		require.NoError(t, err)
		require.Equal(t, os.FileMode(0600), info.Mode().Perm(), f)
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
	require.Error(t, err)
	t.Log("round-trip verified", pulled.Digest)
}
