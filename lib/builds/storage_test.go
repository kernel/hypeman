package builds

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kernel/hypeman/lib/paths"
	"github.com/stretchr/testify/require"
)

func TestBuildStorageRejectsPathTraversal(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), "data")
	p := paths.New(dataDir)
	require.NoError(t, os.MkdirAll(dataDir, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(dataDir, "metadata.json"), []byte(`{"id":"outside"}`), 0644))
	marker := filepath.Join(dataDir, "keep")
	require.NoError(t, os.WriteFile(marker, []byte("keep"), 0644))

	_, err := readMetadata(p, "..")
	require.ErrorIs(t, err, ErrNotFound)
	require.ErrorIs(t, writeMetadata(p, &buildMetadata{ID: ".."}), paths.ErrInvalidPathComponent)
	require.ErrorIs(t, deleteBuild(p, ".."), ErrNotFound)
	require.FileExists(t, marker)
}

func TestBuildMetadataReadWrite_MetadataRoundTrip(t *testing.T) {
	tempDir := t.TempDir()
	p := paths.New(tempDir)
	id := "build-meta-1"

	require.NoError(t, os.MkdirAll(filepath.Join(tempDir, "builds", id), 0755))

	meta := &buildMetadata{
		ID:        id,
		Status:    StatusQueued,
		Tags:      map[string]string{"team": "backend", "env": "staging"},
		CreatedAt: time.Now().UTC().Truncate(time.Second),
	}

	require.NoError(t, writeMetadata(p, meta))

	loaded, err := readMetadata(p, id)
	require.NoError(t, err)
	require.Equal(t, map[string]string{"team": "backend", "env": "staging"}, loaded.Tags)

	build := loaded.toBuild()
	require.Equal(t, map[string]string{"team": "backend", "env": "staging"}, build.Tags)

	loaded.Tags["team"] = "mutated"
	require.Equal(t, "backend", build.Tags["team"])
}
