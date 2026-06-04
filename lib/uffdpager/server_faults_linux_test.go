//go:build linux

package uffdpager

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestSessionReadPageRejectsShortBackingRead(t *testing.T) {
	const pageSize = 4096
	dir := t.TempDir()
	path := filepath.Join(dir, "memory")
	if err := os.WriteFile(path, bytes.Repeat([]byte{1}, pageSize/2), 0644); err != nil {
		t.Fatalf("write backing file: %v", err)
	}

	file, err := os.Open(path)
	if err != nil {
		t.Fatalf("open backing file: %v", err)
	}
	defer file.Close()

	srv := &server{cache: NewPageCache(pageSize)}
	sess := &session{
		cacheKey:    "snapshot-a",
		backingFile: file,
		server:      srv,
	}

	if _, err := sess.readPage(0, pageSize); err == nil {
		t.Fatalf("expected short backing read to fail")
	}
	if _, ok := srv.cache.Get("snapshot-a", 0, pageSize); ok {
		t.Fatalf("short backing read should not be cached")
	}
}
