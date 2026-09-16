package images

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kernel/hypeman/lib/paths"
	"github.com/stretchr/testify/require"
)

func TestReconcileUnreferencedArtifacts(t *testing.T) {
	p := paths.New(t.TempDir())
	liveDigest := strings.Repeat("a", 64)
	deadDigest := strings.Repeat("b", 64)
	for _, digest := range []string{liveDigest, deadDigest} {
		dir := p.ImageLayerDir(digest)
		require.NoError(t, os.MkdirAll(dir, 0755))
		require.NoError(t, os.WriteFile(p.ImageLayerArtifactForFormat(digest, layerArtifactFormat()), []byte("artifact"), 0644))
		require.NoError(t, os.WriteFile(p.ImageLayerRecordForFormat(digest, layerArtifactFormat()), []byte("record"), 0644))
	}

	model := imageManifestModel{
		Layers: []layerDescriptor{{Digest: "sha256:" + liveDigest}},
	}
	data, err := json.Marshal(model)
	require.NoError(t, err)
	manifestPath := p.ImageContentManifestModel(strings.Repeat("c", 64))
	require.NoError(t, os.MkdirAll(filepath.Dir(manifestPath), 0755))
	require.NoError(t, os.WriteFile(manifestPath, data, 0644))

	store := newLayerStore(p, 1)
	require.NoError(t, store.reconcileUnreferencedArtifacts(context.Background()))
	require.DirExists(t, p.ImageLayerDir(liveDigest))
	require.NoDirExists(t, p.ImageLayerDir(deadDigest))
}
