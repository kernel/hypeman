//go:build linux

package instances

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
	const tplID = "tpl-1"

	socketDir := filepath.Join(tplDir, "uffd")
	socketA, err := tracker.acquireUffdForFork(context.Background(), tplID, memPath, socketDir, "fork-a")
	require.NoError(t, err)
	require.NotEmpty(t, socketA)
	require.True(t, tracker.hasFork(tplID, "fork-a"))

	// Second fork against the same template reuses the existing server and
	// returns a distinct UDS path.
	socketB, err := tracker.acquireUffdForFork(context.Background(), tplID, memPath, socketDir, "fork-b")
	require.NoError(t, err)
	require.NotEmpty(t, socketB)
	require.NotEqual(t, socketA, socketB)
	require.True(t, tracker.hasFork(tplID, "fork-b"))

	// Releasing one fork keeps the server alive for the remaining fork.
	require.NoError(t, tracker.releaseUffdForFork(tplID, "fork-a"))
	require.False(t, tracker.hasFork(tplID, "fork-a"))
	require.True(t, tracker.hasFork(tplID, "fork-b"))

	// Releasing the last fork tears the server down.
	require.NoError(t, tracker.releaseUffdForFork(tplID, "fork-b"))
	require.False(t, tracker.hasFork(tplID, "fork-b"))

	// A subsequent acquire should be able to start a fresh server.
	socketC, err := tracker.acquireUffdForFork(context.Background(), tplID, memPath, socketDir, "fork-c")
	require.NoError(t, err)
	require.NotEmpty(t, socketC)
	require.True(t, tracker.hasFork(tplID, "fork-c"))
}

func TestUffdTracker_ReleaseUnknownFork_NoError(t *testing.T) {
	tracker := newUffdTracker()
	require.NoError(t, tracker.releaseUffdForFork("missing-template", "missing-fork"))
}

func TestUffdTracker_AcquireRejectsEmpty(t *testing.T) {
	tracker := newUffdTracker()
	_, err := tracker.acquireUffdForFork(context.Background(), "", "/tmp/x", "/tmp/y", "fork")
	assert.Error(t, err)

	tplDir := t.TempDir()
	memPath := writeUffdTrackerMemFile(t, tplDir)
	_, err = tracker.acquireUffdForFork(context.Background(), "tpl-1", memPath, filepath.Join(tplDir, "uffd"), "")
	assert.Error(t, err)
}

func TestUffdTracker_CloseAll(t *testing.T) {
	tracker := newUffdTracker()
	tplDir := t.TempDir()
	memPath := writeUffdTrackerMemFile(t, tplDir)
	const tplID = "tpl-1"

	_, err := tracker.acquireUffdForFork(context.Background(), tplID, memPath, filepath.Join(tplDir, "uffd"), "fork-a")
	require.NoError(t, err)
	_, err = tracker.acquireUffdForFork(context.Background(), tplID, memPath, filepath.Join(tplDir, "uffd"), "fork-b")
	require.NoError(t, err)

	require.NoError(t, tracker.closeAll())
	assert.False(t, tracker.hasFork(tplID, "fork-a"))
	assert.False(t, tracker.hasFork(tplID, "fork-b"))
}
