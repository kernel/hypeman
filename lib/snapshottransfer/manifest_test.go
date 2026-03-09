package snapshottransfer

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestBuildManifestDeterministic(t *testing.T) {
	guestDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(guestDir, "dir"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(guestDir, "dir", "a.txt"), []byte("alpha"), 0o644); err != nil {
		t.Fatalf("write a.txt: %v", err)
	}
	if err := os.WriteFile(filepath.Join(guestDir, "z.txt"), []byte("zulu"), 0o644); err != nil {
		t.Fatalf("write z.txt: %v", err)
	}
	if err := os.Symlink("dir/a.txt", filepath.Join(guestDir, "link-a")); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	m1, extents1, err := BuildManifest(guestDir, 4)
	if err != nil {
		t.Fatalf("BuildManifest #1: %v", err)
	}
	m2, extents2, err := BuildManifest(guestDir, 4)
	if err != nil {
		t.Fatalf("BuildManifest #2: %v", err)
	}

	if !reflect.DeepEqual(m1, m2) {
		t.Fatalf("manifest should be deterministic")
	}
	if !reflect.DeepEqual(extents1, extents2) {
		t.Fatalf("extents should be deterministic")
	}
	if len(m1.Chunks) == 0 {
		t.Fatalf("expected chunks")
	}

	buf := &bytes.Buffer{}
	first := m1.Chunks[0]
	if err := readDataRange(guestDir, extents1, first.Offset, first.Size, buf); err != nil {
		t.Fatalf("readDataRange: %v", err)
	}
	sum := sha256Hex(buf.Bytes())
	if sum != first.SHA256 {
		t.Fatalf("chunk checksum mismatch: got=%s want=%s", sum, first.SHA256)
	}
}

func TestBuildManifestSparseFileDataSize(t *testing.T) {
	guestDir := t.TempDir()
	path := filepath.Join(guestDir, "sparse.bin")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		t.Fatalf("open sparse file: %v", err)
	}
	if err := f.Truncate(1024 * 1024); err != nil {
		t.Fatalf("truncate sparse file: %v", err)
	}
	if _, err := f.WriteAt([]byte("HEAD"), 0); err != nil {
		t.Fatalf("write head: %v", err)
	}
	if _, err := f.WriteAt([]byte("TAIL"), 1024*1024-4); err != nil {
		t.Fatalf("write tail: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close sparse file: %v", err)
	}

	manifest, _, err := BuildManifest(guestDir, 64*1024)
	if err != nil {
		t.Fatalf("BuildManifest: %v", err)
	}
	if len(manifest.Entries) != 1 {
		t.Fatalf("expected one entry, got %d", len(manifest.Entries))
	}

	entry := manifest.Entries[0]
	if entry.Type != EntryTypeFile {
		t.Fatalf("expected file entry, got %s", entry.Type)
	}
	if entry.Size != 1024*1024 {
		t.Fatalf("expected logical size 1MiB, got %d", entry.Size)
	}

	var extentBytes int64
	for _, ex := range entry.Extents {
		extentBytes += ex.Length
	}
	if extentBytes == 0 || extentBytes > entry.Size {
		t.Fatalf("unexpected extent bytes=%d size=%d", extentBytes, entry.Size)
	}
}

func sha256Hex(b []byte) string {
	h := sha256Sum(b)
	return hex.EncodeToString(h[:])
}

func sha256Sum(b []byte) [32]byte {
	return sha256.Sum256(b)
}
