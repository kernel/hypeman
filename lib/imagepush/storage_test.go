package imagepush

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kernel/hypeman/lib/paths"
	"github.com/stretchr/testify/require"
)

func TestPushStorageRejectsPathTraversal(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), "data")
	p := paths.New(dataDir)
	require.NoError(t, os.MkdirAll(dataDir, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(dataDir, "metadata.json"), []byte(`{"id":"outside"}`), 0644))

	marker := filepath.Join(dataDir, "keep")
	require.NoError(t, os.WriteFile(marker, []byte("keep"), 0644))
	_, err := readMetadata(p, "..")
	require.ErrorIs(t, err, ErrNotFound)
	require.ErrorIs(t, writeMetadata(p, &pushMetadata{ID: ".."}), paths.ErrInvalidPathComponent)
	require.ErrorIs(t, removePushData(p, ".."), ErrNotFound)
	require.FileExists(t, marker)
}

func TestPushMetadataRoundTrip(t *testing.T) {
	p := paths.New(t.TempDir())

	errMsg := "registry unreachable"
	completed := time.Now().Truncate(time.Second)
	meta := &pushMetadata{
		ID:          "push-1",
		Status:      StatusFailed,
		Image:       "docker.io/library/alpine:latest",
		Digest:      "sha256:abc123",
		Target:      "registry.example.com/app:v1",
		Insecure:    true,
		Error:       &errMsg,
		Layers:      3,
		Bytes:       1234,
		CreatedAt:   completed.Add(-time.Minute),
		CompletedAt: &completed,
	}

	if err := writeMetadata(p, meta); err != nil {
		t.Fatalf("writeMetadata: %v", err)
	}

	got, err := readMetadata(p, "push-1")
	if err != nil {
		t.Fatalf("readMetadata: %v", err)
	}
	if got.Status != StatusFailed || got.Image != meta.Image || got.Target != meta.Target || !got.Insecure {
		t.Errorf("round trip mismatch: %+v", got)
	}
	if got.Error == nil || *got.Error != errMsg {
		t.Errorf("error = %v, want %q", got.Error, errMsg)
	}
	if got.Layers != 3 || got.Bytes != 1234 {
		t.Errorf("layers/bytes = %d/%d, want 3/1234", got.Layers, got.Bytes)
	}
	if got.CompletedAt == nil || !got.CompletedAt.Equal(completed) {
		t.Errorf("completed at = %v, want %v", got.CompletedAt, completed)
	}
}

func TestReadMetadataNotFound(t *testing.T) {
	p := paths.New(t.TempDir())

	_, err := readMetadata(p, "missing")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestListPushesSkipsUnreadableMetadata(t *testing.T) {
	p := paths.New(t.TempDir())
	now := time.Now()

	if err := writeMetadata(p, &pushMetadata{ID: "good", Status: StatusPushed, Digest: "sha256:1", Target: "t1", CreatedAt: now}); err != nil {
		t.Fatalf("writeMetadata(good): %v", err)
	}
	// A corrupt record must not fail the whole listing, but must not vanish
	// silently either: it is skipped with a warning so it stays visible.
	badDir := p.PushDir("corrupt")
	if err := os.MkdirAll(badDir, 0755); err != nil {
		t.Fatalf("mkdir corrupt: %v", err)
	}
	if err := os.WriteFile(p.PushMetadata("corrupt"), []byte("{not json"), 0644); err != nil {
		t.Fatalf("write corrupt metadata: %v", err)
	}

	all, err := listAllPushes(p)
	if err != nil {
		t.Fatalf("listAllPushes: %v", err)
	}
	if len(all) != 1 || all[0].ID != "good" {
		t.Errorf("all = %v, want only good", all)
	}
}

func TestListPushesOrderingAndPendingFilter(t *testing.T) {
	p := paths.New(t.TempDir())
	now := time.Now()

	metas := []*pushMetadata{
		{ID: "old-done", Status: StatusPushed, Digest: "sha256:1", Target: "t1", CreatedAt: now.Add(-3 * time.Minute)},
		{ID: "mid-queued", Status: StatusQueued, Digest: "sha256:2", Target: "t2", CreatedAt: now.Add(-2 * time.Minute)},
		{ID: "new-failed", Status: StatusFailed, Digest: "sha256:3", Target: "t3", CreatedAt: now.Add(-1 * time.Minute)},
	}
	for _, meta := range metas {
		if err := writeMetadata(p, meta); err != nil {
			t.Fatalf("writeMetadata(%s): %v", meta.ID, err)
		}
	}

	all, err := listAllPushes(p)
	if err != nil {
		t.Fatalf("listAllPushes: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("len(all) = %d, want 3", len(all))
	}
	if all[0].ID != "new-failed" || all[2].ID != "old-done" {
		t.Errorf("ordering = %s..%s, want newest first", all[0].ID, all[2].ID)
	}

	pending, err := listPendingPushes(p)
	if err != nil {
		t.Fatalf("listPendingPushes: %v", err)
	}
	if len(pending) != 1 || pending[0].ID != "mid-queued" {
		t.Errorf("pending = %v, want only mid-queued", pending)
	}
}
