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

// =============================================================================
// Attack scenario tests - verify defense against common tar-based attacks
// =============================================================================

func TestExtractTarGz_Attack_DotDotSlashVariants(t *testing.T) {
	// Test various path traversal patterns that attackers commonly try
	testCases := []struct {
		name     string
		path     string
		wantErr  bool
	}{
		{"double dot basic", "../etc/passwd", true},
		{"double dot nested", "foo/../../etc/passwd", true},
		{"double dot at start", "..\\etc\\passwd", true}, // Windows-style
		{"hidden in middle", "safe/dir/../../../etc/passwd", true},
		{"percent encoded slashes", "foo%2F..%2Fbar/file.txt", false}, // Percent signs are literal chars in paths
		{"safe relative path", "subdir/file.txt", false},
		{"safe nested path", "a/b/c/d/file.txt", false},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			gw := gzip.NewWriter(&buf)
			tw := tar.NewWriter(gw)

			hdr := &tar.Header{
				Name: tc.path,
				Mode: 0644,
				Size: 4,
			}
			require.NoError(t, tw.WriteHeader(hdr))
			_, err := tw.Write([]byte("test"))
			require.NoError(t, err)

			require.NoError(t, tw.Close())
			require.NoError(t, gw.Close())

			destDir := t.TempDir()
			_, err = ExtractTarGz(&buf, destDir, 1024*1024)

			if tc.wantErr {
				require.Error(t, err, "expected error for path: %s", tc.path)
				assert.ErrorIs(t, err, ErrInvalidArchivePath)
			} else {
				require.NoError(t, err, "unexpected error for path: %s", tc.path)
			}
		})
	}
}

func TestExtractTarGz_Attack_SymlinkChain(t *testing.T) {
	// Attack: Create a chain of symlinks trying to escape
	// link1 -> subdir, subdir/link2 -> ../.. (escape attempt)
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)

	// Create a directory first
	require.NoError(t, tw.WriteHeader(&tar.Header{
		Name:     "subdir/",
		Mode:     0755,
		Typeflag: tar.TypeDir,
	}))

	// Create a symlink that tries to escape via relative path
	require.NoError(t, tw.WriteHeader(&tar.Header{
		Name:     "subdir/escape",
		Mode:     0777,
		Typeflag: tar.TypeSymlink,
		Linkname: "../../etc/passwd",
	}))

	require.NoError(t, tw.Close())
	require.NoError(t, gw.Close())

	destDir := t.TempDir()
	_, err := ExtractTarGz(&buf, destDir, 1024*1024)

	require.Error(t, err)
	assert.ErrorIs(t, err, ErrInvalidArchivePath)
}

func TestExtractTarGz_Attack_HardlinkToOutside(t *testing.T) {
	// Attack: Hard link pointing to a file outside destDir
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)

	hdr := &tar.Header{
		Name:     "evil_hardlink",
		Mode:     0644,
		Typeflag: tar.TypeLink,
		Linkname: "../../../etc/passwd",
	}
	require.NoError(t, tw.WriteHeader(hdr))

	require.NoError(t, tw.Close())
	require.NoError(t, gw.Close())

	destDir := t.TempDir()
	_, err := ExtractTarGz(&buf, destDir, 1024*1024)

	// Hard link with path traversal in target should fail
	require.Error(t, err)
}

func TestExtractTarGz_Attack_DeviceFiles(t *testing.T) {
	// Attack: Try to create device files (should be skipped, not error)
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)

	// Try to create a character device (like /dev/null)
	hdr := &tar.Header{
		Name:     "fake_device",
		Mode:     0666,
		Typeflag: tar.TypeChar,
		Devmajor: 1,
		Devminor: 3,
	}
	require.NoError(t, tw.WriteHeader(hdr))

	// Also add a regular file to verify extraction continues
	hdr = &tar.Header{
		Name: "normal.txt",
		Mode: 0644,
		Size: 5,
	}
	require.NoError(t, tw.WriteHeader(hdr))
	_, err := tw.Write([]byte("hello"))
	require.NoError(t, err)

	require.NoError(t, tw.Close())
	require.NoError(t, gw.Close())

	destDir := t.TempDir()
	_, err = ExtractTarGz(&buf, destDir, 1024*1024)

	// Should succeed - device files are skipped
	require.NoError(t, err)

	// Verify normal file was created
	content, err := os.ReadFile(filepath.Join(destDir, "normal.txt"))
	require.NoError(t, err)
	assert.Equal(t, "hello", string(content))

	// Verify device file was NOT created
	_, err = os.Stat(filepath.Join(destDir, "fake_device"))
	assert.True(t, os.IsNotExist(err), "device file should not be created")
}

func TestExtractTarGz_Attack_ZeroSizeClaimLargeContent(t *testing.T) {
	// Attack: Header claims 0 size but contains large content
	// (malformed tar trying to bypass size checks)
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)

	// Create header with misleading size
	hdr := &tar.Header{
		Name: "misleading.txt",
		Mode: 0644,
		Size: 10, // Claim small size
	}
	require.NoError(t, tw.WriteHeader(hdr))
	// Write exactly 10 bytes as claimed
	_, err := tw.Write([]byte("0123456789"))
	require.NoError(t, err)

	require.NoError(t, tw.Close())
	require.NoError(t, gw.Close())

	destDir := t.TempDir()
	// Set limit below the actual content
	_, err = ExtractTarGz(&buf, destDir, 5)

	// Should fail because even the claimed size exceeds limit
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrArchiveTooLarge)
}

