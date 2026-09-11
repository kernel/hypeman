package images

import (
	"archive/tar"
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/kernel/hypeman/lib/paths"
	"github.com/stretchr/testify/require"
	"golang.org/x/sync/errgroup"
	"golang.org/x/sys/unix"
)

// Smoke tests for layer-artifact edge cases beyond the standard suite.
// Media types, path validation, singleflight sharing, and device/fifo
// entries (root only).

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
	m := testLayerArtifactManager(p)
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

	_, err := m.materializeLayerArtifact(context.Background(), layerDescriptor{
		Digest:    "notadigest",
		MediaType: testTarGzMediaType,
	})
	require.ErrorContains(t, err, "invalid layer digest")
}

func TestSmokeConcurrentMaterializeSharesFlight(t *testing.T) {
	p, m, desc := layerArtifactFixture(t, FormatExt4)

	const callers = 8
	records := make([]*layerArtifact, callers)
	start := time.Now()
	group, groupCtx := errgroup.WithContext(context.Background())
	for i := 0; i < callers; i++ {
		group.Go(func() error {
			record, err := m.materializeLayerArtifact(groupCtx, desc)
			records[i] = record
			return err
		})
	}
	require.NoError(t, group.Wait())
	for i := range records {
		require.Equal(t, records[0], records[i], "all callers must share one build result")
	}
	t.Logf("8 callers finished in %s sharing one flight", time.Since(start))
	layerHex := desc.Digest[len("sha256:"):]
	require.FileExists(t, p.ImageLayerArtifactForFormat(layerHex, layerArtifactFormat()))
}

func TestSmokeDeviceAndFifoEntries(t *testing.T) {
	if !probeLayerArtifactSupport(t.TempDir()) {
		t.Skip("device and fifo tar entries need mknod")
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
