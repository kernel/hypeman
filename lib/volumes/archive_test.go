package volumes

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// createTestTarGz creates a tar.gz archive with the given files
func createTestTarGz(t *testing.T, files map[string][]byte) *bytes.Buffer {
	t.Helper()

	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)

	for name, content := range files {
		hdr := &tar.Header{
			Name: name,
			Mode: 0644,
			Size: int64(len(content)),
		}
		require.NoError(t, tw.WriteHeader(hdr))
		_, err := tw.Write(content)
		require.NoError(t, err)
	}

	require.NoError(t, tw.Close())
	require.NoError(t, gw.Close())

	return &buf
}

func TestExtractTarGz_Basic(t *testing.T) {
	// Create a simple archive
	files := map[string][]byte{
		"hello.txt":     []byte("Hello, World!"),
		"dir/nested.txt": []byte("Nested content"),
	}
	archive := createTestTarGz(t, files)

	// Extract to temp dir
	destDir := t.TempDir()
	extracted, err := ExtractTarGz(archive, destDir, 1024*1024) // 1MB limit

	require.NoError(t, err)
	assert.Equal(t, int64(len("Hello, World!")+len("Nested content")), extracted)

	// Verify files were extracted
	content, err := os.ReadFile(filepath.Join(destDir, "hello.txt"))
	require.NoError(t, err)
	assert.Equal(t, "Hello, World!", string(content))

	content, err = os.ReadFile(filepath.Join(destDir, "dir/nested.txt"))
	require.NoError(t, err)
	assert.Equal(t, "Nested content", string(content))
}

func TestExtractTarGz_SizeLimitExceeded(t *testing.T) {
	// Create an archive with content that exceeds the limit
	files := map[string][]byte{
		"large.txt": bytes.Repeat([]byte("x"), 1000),
	}
	archive := createTestTarGz(t, files)

	destDir := t.TempDir()
	_, err := ExtractTarGz(archive, destDir, 500) // 500 byte limit

	require.Error(t, err)
	assert.ErrorIs(t, err, ErrArchiveTooLarge)
}

func TestExtractTarGz_PathTraversal(t *testing.T) {
	// Create archive with path traversal attempt
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)

	hdr := &tar.Header{
		Name: "../../../etc/passwd",
		Mode: 0644,
		Size: 4,
	}
	require.NoError(t, tw.WriteHeader(hdr))
	_, err := tw.Write([]byte("evil"))
	require.NoError(t, err)

	require.NoError(t, tw.Close())
	require.NoError(t, gw.Close())

	destDir := t.TempDir()
	_, err = ExtractTarGz(&buf, destDir, 1024*1024)

	require.Error(t, err)
	assert.ErrorIs(t, err, ErrInvalidArchivePath)
}

func TestExtractTarGz_AbsolutePath(t *testing.T) {
	// Create archive with absolute path
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)

	hdr := &tar.Header{
		Name: "/etc/passwd",
		Mode: 0644,
		Size: 4,
	}
	require.NoError(t, tw.WriteHeader(hdr))
	_, err := tw.Write([]byte("evil"))
	require.NoError(t, err)

	require.NoError(t, tw.Close())
	require.NoError(t, gw.Close())

	destDir := t.TempDir()
	_, err = ExtractTarGz(&buf, destDir, 1024*1024)

	require.Error(t, err)
	assert.ErrorIs(t, err, ErrInvalidArchivePath)
}

func TestExtractTarGz_Symlink(t *testing.T) {
	// Create archive with a valid symlink
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)

	// Add a regular file first
	hdr := &tar.Header{
		Name: "target.txt",
		Mode: 0644,
		Size: 5,
	}
	require.NoError(t, tw.WriteHeader(hdr))
	_, err := tw.Write([]byte("hello"))
	require.NoError(t, err)

	// Add a valid symlink
	hdr = &tar.Header{
		Name:     "link.txt",
		Mode:     0777,
		Typeflag: tar.TypeSymlink,
		Linkname: "target.txt",
	}
	require.NoError(t, tw.WriteHeader(hdr))

	require.NoError(t, tw.Close())
	require.NoError(t, gw.Close())

	destDir := t.TempDir()
	_, err = ExtractTarGz(&buf, destDir, 1024*1024)
	require.NoError(t, err)

	// Verify symlink was created
	linkPath := filepath.Join(destDir, "link.txt")
	target, err := os.Readlink(linkPath)
	require.NoError(t, err)
	assert.Equal(t, "target.txt", target)
}

func TestExtractTarGz_SymlinkEscape(t *testing.T) {
	// Create archive with symlink that escapes destination
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)

	hdr := &tar.Header{
		Name:     "escape.txt",
		Mode:     0777,
		Typeflag: tar.TypeSymlink,
		Linkname: "../../etc/passwd",
	}
	require.NoError(t, tw.WriteHeader(hdr))

	require.NoError(t, tw.Close())
	require.NoError(t, gw.Close())

	destDir := t.TempDir()
	_, err := ExtractTarGz(&buf, destDir, 1024*1024)

	require.Error(t, err)
	assert.ErrorIs(t, err, ErrInvalidArchivePath)
}

func TestExtractTarGz_AbsoluteSymlink(t *testing.T) {
	// Create archive with absolute symlink target
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)

	hdr := &tar.Header{
		Name:     "abs.txt",
		Mode:     0777,
		Typeflag: tar.TypeSymlink,
		Linkname: "/etc/passwd",
	}
	require.NoError(t, tw.WriteHeader(hdr))

	require.NoError(t, tw.Close())
	require.NoError(t, gw.Close())

	destDir := t.TempDir()
	_, err := ExtractTarGz(&buf, destDir, 1024*1024)

	require.Error(t, err)
	assert.ErrorIs(t, err, ErrInvalidArchivePath)
}

func TestExtractTarGz_PreventsTarBomb(t *testing.T) {
	// Create a "tar bomb" - many small files that together exceed the limit
	files := make(map[string][]byte)
	for i := 0; i < 100; i++ {
		// Use unique file names (file_000.txt, file_001.txt, etc.)
		files[fmt.Sprintf("dir/file_%03d.txt", i)] = bytes.Repeat([]byte("x"), 100)
	}
	archive := createTestTarGz(t, files)

	destDir := t.TempDir()
	_, err := ExtractTarGz(archive, destDir, 5000) // 5KB limit, but archive has 10KB

	require.Error(t, err)
	assert.ErrorIs(t, err, ErrArchiveTooLarge)
}

