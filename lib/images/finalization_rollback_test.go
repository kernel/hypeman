package images

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/kernel/hypeman/lib/paths"
	"github.com/stretchr/testify/require"
)

func TestFinalizationRollbackPreservesUninstalledManifest(t *testing.T) {
	p := paths.New(t.TempDir())
	m := &manager{paths: p}
	ref, err := ParseNormalizedRef("localhost/qa@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	require.NoError(t, err)
	resolved := NewResolvedRef(ref, ref.Digest())
	require.NoError(t, writeMetadata(p, ref.Repository(), ref.DigestHex(), &imageMetadata{BuildID: "qa-build", Status: StatusPending}))
	layout := resolveImageLayout(p, ref.Repository(), ref.DigestHex())
	modelPath := manifestModelPath(p, layout, ref.DigestHex())
	require.NoError(t, os.MkdirAll(filepath.Dir(modelPath), 0700))
	require.NoError(t, os.WriteFile(modelPath, []byte("preexisting-manifest"), 0600))
	stagedDisk := filepath.Join(layout.dir, "qa-staged-disk")
	require.NoError(t, os.WriteFile(stagedDisk, []byte("synthetic-disk"), 0600))
	// Manifest validation fails before any model file is installed.
	err = m.finalizeImage(resolved, &pullResult{Metadata: &containerMetadata{OS: "linux", Architecture: "arm64"}, Manifest: &imageManifestModel{}}, "qa-build", stagedImageFiles{disk: stagedDisk, sizeBytes: 14})
	require.ErrorContains(t, err, "write manifest model")
	data, err := os.ReadFile(modelPath)
	require.NoError(t, err, "rollback must not remove a manifest that this attempt did not install")
	require.Equal(t, "preexisting-manifest", string(data))
}
