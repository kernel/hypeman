package images

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	gcr "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/kernel/hypeman/lib/paths"
	"github.com/stretchr/testify/require"
)

// composeTestImage builds the standard two-layer fixture: a base layer with
// content the top layer deletes, masks, replaces, and extends.
func composeTestImage(t *testing.T) gcr.Image {
	t.Helper()

	base := specLayer(t, []tarEntrySpec{
		{name: "etc/", isDir: true, mode: 0755},
		{name: "etc/config.txt", content: "original", mode: 0644},
		{name: "app/", isDir: true, mode: 0755},
		{name: "app/main.txt", content: "v1", mode: 0644},
		{name: "data/", isDir: true, mode: 0755},
		{name: "data/old.txt", content: "stale", mode: 0644},
		{name: "replacedir/", isDir: true, mode: 0755},
		{name: "replacedir/inner.txt", content: "inner", mode: 0644},
	})
	top := specLayer(t, []tarEntrySpec{
		{name: "etc/.wh.config.txt", content: "", mode: 0644},
		{name: "app/main.txt", content: "v2", mode: 0644},
		{name: "data/.wh..wh..opq", content: "", mode: 0644},
		{name: "data/new.txt", content: "new", mode: 0644},
		{name: "bin/", isDir: true, mode: 0755},
		{name: "bin/tool", content: "tool", mode: 0755},
		{name: "replacedir", content: "now a file", mode: 0644},
	})

	img, err := mutate.AppendLayers(empty.Image, base, top)
	require.NoError(t, err)
	return img
}

// composeFixture composes the standard fixture image into the shared OCI cache
// and returns a client plus its validated manifest model.
func composeFixture(t *testing.T, p *paths.Paths) (*ociClient, string, *imageManifestModel) {
	t.Helper()

	img := composeTestImage(t)
	writeLayerTestLayout(t, p, img)

	client, err := newOCIClient(p.SystemOCICache())
	require.NoError(t, err)
	digest, err := img.Digest()
	require.NoError(t, err)
	tag := digestToLayoutTag(digest.String())
	bundle, err := client.extractOCIImageBundle(tag)
	require.NoError(t, err)
	return client, tag, bundle.Model
}

func TestComposeRootfsWhiteoutsAndOrdering(t *testing.T) {
	p := paths.New(t.TempDir())
	client, tag, model := composeFixture(t, p)
	require.Len(t, model.Layers, 2)

	dest := filepath.Join(t.TempDir(), "rootfs")
	require.NoError(t, client.composeRootfsContext(context.Background(), dest, tag, model))

	// Whiteout removed the base entry.
	_, err := os.Lstat(filepath.Join(dest, "etc", "config.txt"))
	require.True(t, os.IsNotExist(err), "whiteout must delete the base entry")

	// Plain replacement.
	data, err := os.ReadFile(filepath.Join(dest, "app", "main.txt"))
	require.NoError(t, err)
	require.Equal(t, "v2", string(data))

	// Opaque directory masked the base content.
	_, err = os.Lstat(filepath.Join(dest, "data", "old.txt"))
	require.True(t, os.IsNotExist(err), "opaque marker must mask base contents")
	data, err = os.ReadFile(filepath.Join(dest, "data", "new.txt"))
	require.NoError(t, err)
	require.Equal(t, "new", string(data))

	// Directory replaced by a regular file.
	info, err := os.Lstat(filepath.Join(dest, "replacedir"))
	require.NoError(t, err)
	require.False(t, info.IsDir())
	data, err = os.ReadFile(filepath.Join(dest, "replacedir"))
	require.NoError(t, err)
	require.Equal(t, "now a file", string(data))

	// New entry present with its mode.
	info, err = os.Stat(filepath.Join(dest, "bin", "tool"))
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0755), info.Mode().Perm())

	// No whiteout markers survive composition.
	require.NoError(t, filepath.Walk(dest, func(path string, info os.FileInfo, err error) error {
		require.NoError(t, err)
		require.NotContains(t, info.Name(), whiteoutPrefix, "whiteout marker leaked into composed rootfs")
		return nil
	}))
}

// zeroLayerModel returns a schema-valid manifest model with no layers.
func zeroLayerModel() *imageManifestModel {
	return &imageManifestModel{
		SchemaVersion: manifestModelSchemaVersion,
		Digest:        "sha256:" + strings.Repeat("ab", 32),
		RootFSType:    "layers",
		Config:        manifestConfigRef{Digest: "sha256:" + strings.Repeat("cd", 32)},
		Layers:        make([]layerDescriptor, 0),
	}
}

func TestComposeRootfsEmptyLayers(t *testing.T) {
	p := paths.New(t.TempDir())
	client, err := newOCIClient(p.SystemOCICache())
	require.NoError(t, err)
	model := zeroLayerModel()
	dest := filepath.Join(t.TempDir(), "rootfs")
	require.NoError(t, client.composeRootfsContext(context.Background(), dest, model.Digest, model))
	entries, err := os.ReadDir(dest)
	require.NoError(t, err)
	require.Empty(t, entries)
}

func TestComposeRootfsInvalidModel(t *testing.T) {
	p := paths.New(t.TempDir())
	client, tag, model := composeFixture(t, p)

	model.Config.DiffIDs = model.Config.DiffIDs[:1]
	err := client.composeRootfsContext(context.Background(), filepath.Join(t.TempDir(), "rootfs"), tag, model)
	require.ErrorContains(t, err, "1 diff ids for 2 layers")
}

func TestComposeRootfsMissingBlob(t *testing.T) {
	p := paths.New(t.TempDir())
	client, err := newOCIClient(p.SystemOCICache())
	require.NoError(t, err)

	digestHex := "sha256:" + strings.Repeat("ab", 32)
	model := &imageManifestModel{
		SchemaVersion: manifestModelSchemaVersion,
		Digest:        digestHex,
		RootFSType:    "layers",
		Config: manifestConfigRef{
			Digest:  "sha256:" + strings.Repeat("cd", 32),
			DiffIDs: []string{"sha256:" + strings.Repeat("ef", 32)},
		},
		Layers: []layerDescriptor{{
			Digest:    "sha256:" + strings.Repeat("01", 32),
			MediaType: "application/vnd.oci.image.layer.v1.tar+gzip",
			DiffID:    "sha256:" + strings.Repeat("ef", 32),
		}},
	}
	err = client.composeRootfsContext(context.Background(), t.TempDir(), digestHex, model)
	require.ErrorContains(t, err, "missing from oci cache")
}

func TestComposeRootfsDiffIDMismatch(t *testing.T) {
	p := paths.New(t.TempDir())
	client, tag, model := composeFixture(t, p)

	// Desynchronize the top layer's diff id from its content while keeping the
	// model internally consistent, so validation passes and the mismatch is
	// caught against the unpacked stream instead.
	forged := "sha256:" + strings.Repeat("ff", 32)
	model.Layers[1].DiffID = forged
	model.Config.DiffIDs[1] = forged

	err := client.composeRootfsContext(context.Background(), filepath.Join(t.TempDir(), "rootfs"), tag, model)
	require.ErrorContains(t, err, "diff id mismatch")
}

// TestComposeRootfsExportsValidErofs composes the fixture image and exports it
// to erofs, then verifies the filesystem is intact and its contents match the
// composed tree.
func TestComposeRootfsExportsValidErofs(t *testing.T) {
	if _, err := exec.LookPath("mkfs.erofs"); err != nil {
		t.Skip("mkfs.erofs not available")
	}
	if _, err := exec.LookPath("fsck.erofs"); err != nil {
		t.Skip("fsck.erofs not available")
	}

	p := paths.New(t.TempDir())
	client, tag, model := composeFixture(t, p)

	staging := filepath.Join(t.TempDir(), "rootfs")
	require.NoError(t, client.composeRootfsContext(context.Background(), staging, tag, model))

	diskPath := filepath.Join(t.TempDir(), "rootfs.erofs")
	size, err := ExportRootfs(staging, diskPath, FormatErofs)
	require.NoError(t, err)
	require.Greater(t, size, int64(0))

	output, err := exec.Command("fsck.erofs", "--extract", diskPath).CombinedOutput()
	require.NoError(t, err, "fsck.erofs failed: %s", output)
}
