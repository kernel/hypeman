//go:build linux

package instances

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/kernel/hypeman/lib/templates"
)

func writeUffdTrackerMemFile(t *testing.T, dir string) string {
	t.Helper()
	p := filepath.Join(dir, "memory")
	require.NoError(t, os.WriteFile(p, make([]byte, 4096), 0o644))
	return p
}

func TestUffdTracker_AcquireAndReleaseLifecycle(t *testing.T) {
	tracker := newUffdTracker()
	t.Cleanup(func() { _ = tracker.closeAll() })

	tplDir := t.TempDir()
	memPath := writeUffdTrackerMemFile(t, tplDir)
	tpl := &templates.Template{ID: "tpl-1", SourceInstanceID: "src-1"}

	socketDir := filepath.Join(tplDir, "uffd")
	socketA, err := tracker.acquireUffdForFork(context.Background(), tpl, memPath, socketDir, "fork-a")
	require.NoError(t, err)
	require.NotEmpty(t, socketA)
	require.True(t, tracker.hasFork(tpl.ID, "fork-a"))

	// Second fork against the same template reuses the existing server and
	// returns a distinct UDS path.
	socketB, err := tracker.acquireUffdForFork(context.Background(), tpl, memPath, socketDir, "fork-b")
	require.NoError(t, err)
	require.NotEmpty(t, socketB)
	require.NotEqual(t, socketA, socketB)
	require.True(t, tracker.hasFork(tpl.ID, "fork-b"))

	// Releasing one fork keeps the server alive for the remaining fork.
	require.NoError(t, tracker.releaseUffdForFork(tpl.ID, "fork-a"))
	require.False(t, tracker.hasFork(tpl.ID, "fork-a"))
	require.True(t, tracker.hasFork(tpl.ID, "fork-b"))

	// Releasing the last fork tears the server down.
	require.NoError(t, tracker.releaseUffdForFork(tpl.ID, "fork-b"))
	require.False(t, tracker.hasFork(tpl.ID, "fork-b"))

	// A subsequent acquire should be able to start a fresh server.
	socketC, err := tracker.acquireUffdForFork(context.Background(), tpl, memPath, socketDir, "fork-c")
	require.NoError(t, err)
	require.NotEmpty(t, socketC)
	require.True(t, tracker.hasFork(tpl.ID, "fork-c"))
}

func TestUffdTracker_ReleaseUnknownFork_NoError(t *testing.T) {
	tracker := newUffdTracker()
	require.NoError(t, tracker.releaseUffdForFork("missing-template", "missing-fork"))
}

func TestUffdTracker_AcquireRejectsEmpty(t *testing.T) {
	tracker := newUffdTracker()
	_, err := tracker.acquireUffdForFork(context.Background(), nil, "/tmp/x", "/tmp/y", "fork")
	assert.Error(t, err)

	tplDir := t.TempDir()
	memPath := writeUffdTrackerMemFile(t, tplDir)
	tpl := &templates.Template{ID: "tpl-1", SourceInstanceID: "src-1"}
	_, err = tracker.acquireUffdForFork(context.Background(), tpl, memPath, filepath.Join(tplDir, "uffd"), "")
	assert.Error(t, err)
}

func TestUffdTracker_CloseAll(t *testing.T) {
	tracker := newUffdTracker()
	tplDir := t.TempDir()
	memPath := writeUffdTrackerMemFile(t, tplDir)
	tpl := &templates.Template{ID: "tpl-1", SourceInstanceID: "src-1"}

	_, err := tracker.acquireUffdForFork(context.Background(), tpl, memPath, filepath.Join(tplDir, "uffd"), "fork-a")
	require.NoError(t, err)
	_, err = tracker.acquireUffdForFork(context.Background(), tpl, memPath, filepath.Join(tplDir, "uffd"), "fork-b")
	require.NoError(t, err)

	require.NoError(t, tracker.closeAll())
	assert.False(t, tracker.hasFork(tpl.ID, "fork-a"))
	assert.False(t, tracker.hasFork(tpl.ID, "fork-b"))
}
