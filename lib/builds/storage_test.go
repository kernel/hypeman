package builds

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kernel/hypeman/lib/paths"
	"github.com/stretchr/testify/assert"
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

func TestListAllBuilds_LogsAndSkipsCorruptMetadata(t *testing.T) {
	tempDir := t.TempDir()
	p := paths.New(tempDir)

	// One valid build and two corrupt entries: unparsable JSON and a
	// directory with no metadata file at all.
	valid := &buildMetadata{ID: "valid-build", Status: StatusReady, CreatedAt: time.Now()}
	require.NoError(t, writeMetadata(p, valid))
	require.NoError(t, os.MkdirAll(p.BuildDir("corrupt-json"), 0755))
	require.NoError(t, os.WriteFile(p.BuildMetadata("corrupt-json"), []byte("{not json"), 0644))
	require.NoError(t, os.MkdirAll(p.BuildDir("missing-file"), 0755))

	var logBuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuf, nil))

	metas, err := listAllBuilds(p, logger)
	require.NoError(t, err)
	require.Len(t, metas, 1)
	assert.Equal(t, "valid-build", metas[0].ID)

	out := logBuf.String()
	assert.Contains(t, out, "corrupt-json")
	assert.Contains(t, out, "missing-file")
}

func TestListPendingBuilds_LogsAndSkipsCorruptMetadata(t *testing.T) {
	tempDir := t.TempDir()
	p := paths.New(tempDir)

	pending := &buildMetadata{ID: "pending-build", Status: StatusQueued, CreatedAt: time.Now()}
	require.NoError(t, writeMetadata(p, pending))
	require.NoError(t, os.MkdirAll(p.BuildDir("corrupt-json"), 0755))
	require.NoError(t, os.WriteFile(p.BuildMetadata("corrupt-json"), []byte("{not json"), 0644))

	var logBuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuf, nil))

	metas, err := listPendingBuilds(p, logger)
	require.NoError(t, err)
	require.Len(t, metas, 1)
	assert.Equal(t, "pending-build", metas[0].ID)
	assert.Contains(t, logBuf.String(), "corrupt-json")
}
