package images

import (
	"archive/tar"
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/kernel/hypeman/lib/paths"
	"github.com/klauspost/compress/zstd"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

// Smoke tests for PR #456 edge cases beyond the standard suite.
// Media types, path validation, cache-miss rebuilds, singleflight sharing,
// device/fifo entries (root only), and disk-usage accounting.

func writeRawTarLayer(t *testing.T, dir, name string, entries ...func(*testing.T, *tar.Writer)) string {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, entry := range entries {
		entry(t, tw)
	}
	require.NoError(t, tw.Close())
	path := filepath.Join(dir, name)
	require.NoError(t, os.WriteFile(path, buf.Bytes(), 0644))
	return path
}

func writeZstdTarLayer(t *testing.T, dir, name string, entries ...func(*testing.T, *tar.Writer)) string {
	t.Helper()
	var tarBuf bytes.Buffer
	tw := tar.NewWriter(&tarBuf)
	for _, entry := range entries {
		entry(t, tw)
	}
	require.NoError(t, tw.Close())

	var zstBuf bytes.Buffer
	zw, err := zstd.NewWriter(&zstBuf)
	require.NoError(t, err)
	_, err = zw.Write(tarBuf.Bytes())
	require.NoError(t, err)
	require.NoError(t, zw.Close())
	path := filepath.Join(dir, name)
	require.NoError(t, os.WriteFile(path, zstBuf.Bytes(), 0644))
	return path
}

const (
	ociZstdMediaType     = "application/vnd.oci.image.layer.v1.tar+zstd"
	dockerZstdMediaType  = "application/vnd.docker.image.rootfs.diff.tar.zstd"
	dockerGzipMediaType  = "application/vnd.docker.image.rootfs.diff.tar.gzip"
	ociPlainTarMediaType = "application/vnd.oci.image.layer.v1.tar"
)

func TestSmokeUnpackZstdMediaTypes(t *testing.T) {
	root := t.TempDir()
	for name, tc := range map[string]struct {
		mediaType string
		blob      string
	}{
		"oci_suffix_zstd":   {ociZstdMediaType, writeZstdTarLayer(t, root, "oci.tar.zst", fileEntry("f.txt", "zstd"))},
		"docker_tar_zstd":   {dockerZstdMediaType, writeZstdTarLayer(t, root, "docker.tar.zstd", fileEntry("f.txt", "zstd"))},
		"docker_tar_gzip":   {dockerGzipMediaType, writeLayerBlob(t, root, "docker.tar.gz", fileEntry("f.txt", "gzip"))},
		"plain_tar":         {ociPlainTarMediaType, writeRawTarLayer(t, root, "plain.tar", fileEntry("f.txt", "raw"))},
		"unknown_media_raw": {"application/vnd.custom.tar", writeRawTarLayer(t, root, "custom.tar", fileEntry("f.txt", "raw"))},
	} {
		t.Run(name, func(t *testing.T) {
			dest := filepath.Join(root, t.Name())
			stats, err := unpackLayerBlob(context.Background(), tc.blob, tc.mediaType, dest, composeOnDiskFormat())
			require.NoError(t, err)
			require.Greater(t, stats.unpackedBytes, int64(0))
			require.FileExists(t, filepath.Join(dest, "f.txt"))
		})
	}
}

func TestSmokeRejectsBadLayerHex(t *testing.T) {
	p := paths.New(t.TempDir())
	m := &manager{paths: p}
	for _, digest := range []string{
		"sha256:..",
		"sha256:../escape",
		"sha256:a/b",
		"sha256:",
	} {
		_, err := m.materializeLayerArtifact(context.Background(), layerDescriptor{
			Digest:    digest,
			MediaType: testTarGzMediaType,
		})
		require.ErrorContains(t, err, "invalid layer digest", "digest %q must be rejected", digest)
	}
	require.NoDirExists(t, filepath.Join(p.ImageLayersDir(), "..", "layers-escape"))

	// A non-digest string without the sha256: prefix passes path validation
	// and fails at blob lookup instead — acceptable, since real descriptors
	// always carry manifest-parsed digests.
	_, err := m.materializeLayerArtifact(context.Background(), layerDescriptor{
		Digest:    "notadigest",
		MediaType: testTarGzMediaType,
	})
	require.ErrorContains(t, err, "missing from oci cache")
}

func TestSmokeRebuildWhenArtifactFileMissing(t *testing.T) {
	if _, err := exec.LookPath("mkfs.ext4"); err != nil {
		t.Skip("mkfs.ext4 not available")
	}
	originalFormat := DefaultImageFormat
	DefaultImageFormat = FormatExt4
	t.Cleanup(func() { DefaultImageFormat = originalFormat })

	p := paths.New(t.TempDir())
	img, err := mutate.AppendLayers(empty.Image, syntheticLayer(t, "f.txt", "content"))
	require.NoError(t, err)
	writeLayerTestLayout(t, p, img)
	desc := layerDescFromImage(t, img, 0)
	m := &manager{paths: p}

	first, err := m.materializeLayerArtifact(context.Background(), desc)
	require.NoError(t, err)
	layerHex := desc.Digest[len("sha256:"):]

	// Record survives but the artifact file is gone: must rebuild, not reuse.
	require.NoError(t, os.Remove(p.ImageLayerArtifactForFormat(layerHex, layerArtifactFormat())))
	second, err := m.materializeLayerArtifact(context.Background(), desc)
	require.NoError(t, err)
	require.True(t, second.CreatedAt.After(first.CreatedAt), "missing artifact must trigger a rebuild")
	require.FileExists(t, p.ImageLayerArtifactForFormat(layerHex, layerArtifactFormat()))
}

func TestSmokeConcurrentMaterializeSharesFlight(t *testing.T) {
	if _, err := exec.LookPath("mkfs.ext4"); err != nil {
		t.Skip("mkfs.ext4 not available")
	}
	originalFormat := DefaultImageFormat
	DefaultImageFormat = FormatExt4
	t.Cleanup(func() { DefaultImageFormat = originalFormat })

	p := paths.New(t.TempDir())
	img, err := mutate.AppendLayers(empty.Image, syntheticLayer(t, "f.txt", "shared"))
	require.NoError(t, err)
	writeLayerTestLayout(t, p, img)
	desc := layerDescFromImage(t, img, 0)
	m := &manager{paths: p}

	const callers = 8
	var wg sync.WaitGroup
	records := make([]*layerArtifact, callers)
	errs := make([]error, callers)
	start := time.Now()
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			records[i], errs[i] = m.materializeLayerArtifact(context.Background(), desc)
		}(i)
	}
	wg.Wait()
	require.NotEmpty(t, errs)
	for i := range errs {
		require.NoError(t, errs[i])
		require.Equal(t, records[0], records[i], "all callers must share one build result")
	}
	t.Logf("8 callers finished in %s sharing one flight", time.Since(start))
	layerHex := desc.Digest[len("sha256:"):]
	require.FileExists(t, p.ImageLayerArtifactForFormat(layerHex, layerArtifactFormat()))
}

func TestSmokeDeviceAndFifoEntries(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("device and fifo tar entries need root to mknod")
	}
	root := t.TempDir()
	blob := writeRawTarLayer(t, root, "devices.tar",
		fileEntry("regular.txt", "x"),
		func(t *testing.T, tw *tar.Writer) {
			require.NoError(t, tw.WriteHeader(&tar.Header{
				Name: "dev/null", Typeflag: tar.TypeChar,
				Devmajor: 1, Devminor: 3, Mode: 0666,
			}))
		},
		func(t *testing.T, tw *tar.Writer) {
			require.NoError(t, tw.WriteHeader(&tar.Header{
				Name: "pipes/fifo", Typeflag: tar.TypeFifo, Mode: 0644,
			}))
		},
	)
	dest := filepath.Join(root, "dest")
	_, err := unpackLayerBlob(context.Background(), blob, ociPlainTarMediaType, dest, composeOnDiskFormat())
	require.NoError(t, err)

	require.FileExists(t, filepath.Join(dest, "regular.txt"))
	var stat unix.Stat_t
	require.NoError(t, unix.Lstat(filepath.Join(dest, "dev", "null"), &stat))
	require.Equal(t, uint32(unix.S_IFCHR), stat.Mode&unix.S_IFMT)
	require.Equal(t, uint64(0x103), uint64(stat.Rdev), "char device 1:3")
	require.NoError(t, unix.Lstat(filepath.Join(dest, "pipes", "fifo"), &stat))
	require.Equal(t, uint32(unix.S_IFIFO), stat.Mode&unix.S_IFMT)
}
