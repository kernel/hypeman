package builds

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kernel/hypeman/lib/paths"
	"github.com/stretchr/testify/require"
)

func TestBuildMetadataReadWrite_MetadataRoundTrip(t *testing.T) {
	tempDir := t.TempDir()
	p := paths.New(tempDir)
	id := "build-meta-1"

	require.NoError(t, os.MkdirAll(filepath.Join(tempDir, "builds", id), 0755))

	meta := &buildMetadata{
		ID:        id,
		Status:    StatusQueued,
		Metadata:  map[string]string{"team": "backend", "env": "staging"},
		CreatedAt: time.Now().UTC().Truncate(time.Second),
	}

	require.NoError(t, writeMetadata(p, meta))

	loaded, err := readMetadata(p, id)
	require.NoError(t, err)
	require.Equal(t, map[string]string{"team": "backend", "env": "staging"}, loaded.Metadata)

	build := loaded.toBuild()
	require.Equal(t, map[string]string{"team": "backend", "env": "staging"}, build.Metadata)

	loaded.Metadata["team"] = "mutated"
	require.Equal(t, "backend", build.Metadata["team"])
}
