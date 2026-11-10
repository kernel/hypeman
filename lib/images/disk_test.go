package images

import (
	"archive/tar"
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"testing"
	"time"

	gcr "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/layout"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var (
	dumpErofsOwner     = regexp.MustCompile(`Uid: (\d+)\s+Gid: (\d+)\s+Access: (\d+)`)
	dumpErofsTimestamp = regexp.MustCompile(`Timestamp: \S+ \S+\.(\d+)`)
)

func TestExportImageRootfsKeepsImageOwners(t *testing.T) {
	for _, binary := range []string{"mkfs.erofs", "dump.erofs"} {
		if _, err := exec.LookPath(binary); err != nil {
			t.Skipf("%s not available", binary)
		}
	}
	if os.Geteuid() != 0 && !mkfsErofsReadsTarMtimes() {
		t.Skip("rootless export needs mkfs.erofs 1.8 or newer")
	}

	mtime := time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC)
	layerTar := tarGz(t, func(tw *tar.Writer) {
		entries := []struct {
			header  tar.Header
			content string
		}{
			{tar.Header{Name: "etc/", Typeflag: tar.TypeDir, Mode: 0755}, ""},
			{tar.Header{Name: "etc/shadow", Typeflag: tar.TypeReg, Mode: 0640, Gid: 42}, "secret"},
			{tar.Header{Name: "home/", Typeflag: tar.TypeDir, Mode: 0755}, ""},
			{tar.Header{Name: "home/app/", Typeflag: tar.TypeDir, Mode: 0700, Uid: 1000, Gid: 1000}, ""},
			{tar.Header{Name: "home/app/data", Typeflag: tar.TypeReg, Mode: 0600, Uid: 1000, Gid: 1000}, "data"},
			{tar.Header{Name: "home/app/data-link", Typeflag: tar.TypeLink, Linkname: "home/app/data"}, ""},
		}
		for _, entry := range entries {
			entry.header.ModTime = mtime
			writeTarEntry(t, tw, &entry.header, entry.content)
		}
	})
	layer, err := tarball.LayerFromOpener(func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(layerTar)), nil
	})
	require.NoError(t, err)
	img, err := mutate.AppendLayers(empty.Image, layer)
	require.NoError(t, err)
	diffID, err := layer.DiffID()
	require.NoError(t, err)
	img, err = mutate.ConfigFile(img, &gcr.ConfigFile{
		Architecture: "amd64",
		OS:           "linux",
		RootFS:       gcr.RootFS{Type: "layers", DiffIDs: []gcr.Hash{diffID}},
	})
	require.NoError(t, err)

	cacheDir := t.TempDir()
	layoutPath, err := layout.Write(cacheDir, empty.Index)
	require.NoError(t, err)
	require.NoError(t, layoutPath.AppendImage(img, layout.WithAnnotations(map[string]string{
		"org.opencontainers.image.ref.name": "owners",
	})))
	client, err := newOCIClient(cacheDir)
	require.NoError(t, err)

	rootfsDir := filepath.Join(t.TempDir(), "rootfs")
	require.NoError(t, client.unpackLayers(context.Background(), "owners", rootfsDir))
	diskPath := filepath.Join(t.TempDir(), "rootfs.erofs")
	_, err = exportImageRootfs(context.Background(), rootfsDir, diskPath, FormatErofs)
	require.NoError(t, err)

	for _, tc := range []struct {
		path, uid, gid, mode string
	}{
		{"/", "0", "0", "0755"},
		{"/etc/shadow", "0", "42", "0640"},
		{"/home/app", "1000", "1000", "0700"},
		{"/home/app/data", "1000", "1000", "0600"},
		{"/home/app/data-link", "1000", "1000", "0600"},
	} {
		output, err := exec.Command("dump.erofs", "--path="+tc.path, diskPath).CombinedOutput()
		require.NoError(t, err, string(output))
		owner := dumpErofsOwner.FindSubmatch(output)
		require.NotNil(t, owner, string(output))
		assert.Equal(t, []string{tc.uid, tc.gid, tc.mode}, []string{string(owner[1]), string(owner[2]), string(owner[3])}, tc.path)
		if tc.path != "/" {
			timestamp := dumpErofsTimestamp.FindSubmatch(output)
			require.NotNil(t, timestamp, string(output))
			assert.Equal(t, "000000000", string(timestamp[1]), "%s mtime nanoseconds", tc.path)
		}
	}
}
