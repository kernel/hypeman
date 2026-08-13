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

	require.NoError(t, os.MkdirAll(filepath.Join(dataDir, "logs"), 0755))
	require.NoError(t, os.WriteFile(filepath.Join(dataDir, "logs", "build.log"), []byte("outside"), 0644))
	require.NoError(t, os.WriteFile(filepath.Join(dataDir, "config.json"), []byte(`{}`), 0644))

	tests := []struct {
		name string
		run  func() error
		want error
	}{
		{"read metadata", func() error { _, err := readMetadata(p, ".."); return err }, ErrNotFound},
		{"write metadata", func() error { return writeMetadata(p, &buildMetadata{ID: ".."}) }, paths.ErrInvalidPathComponent},
		{"delete build", func() error { return deleteBuild(p, "..") }, ErrNotFound},
		{"append log", func() error { return appendLog(p, "..", []byte("data")) }, paths.ErrInvalidPathComponent},
		{"write log", func() error { return writeLog(p, "..", []byte("data")) }, paths.ErrInvalidPathComponent},
		{"read log", func() error { _, err := readLog(p, ".."); return err }, ErrNotFound},
		{"write config", func() error { return writeBuildConfig(p, "..", &BuildConfig{}) }, paths.ErrInvalidPathComponent},
		{"read config", func() error { _, err := readBuildConfig(p, ".."); return err }, ErrNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.ErrorIs(t, tt.run(), tt.want)
		})
	}
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
