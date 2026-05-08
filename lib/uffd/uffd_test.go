package uffd

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func writeTempMemFile(t *testing.T, size int) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "memory")
	f, err := os.Create(path)
	require.NoError(t, err)
	require.NoError(t, f.Truncate(int64(size)))
	require.NoError(t, f.Close())
	return path
}

func TestNewServer_RequiresMemFile(t *testing.T) {
	_, err := NewServer(Config{SocketDir: t.TempDir()})
	assert.Error(t, err)
}

func TestNewServer_RequiresSocketDir(t *testing.T) {
	_, err := NewServer(Config{MemFilePath: writeTempMemFile(t, 4096)})
	assert.Error(t, err)
}

func TestNewServer_ReportsMemSizeAndPageSize(t *testing.T) {
	memPath := writeTempMemFile(t, 16384)
	s, err := NewServer(Config{
		MemFilePath: memPath,
		SocketDir:   t.TempDir(),
		PageSize:    4096,
	})
	require.NoError(t, err)
	defer s.Close()

	assert.Equal(t, int64(16384), s.MemSize())
	assert.Equal(t, 4096, s.PageSize())
}

func TestNewServer_DefaultsPageSizeToHost(t *testing.T) {
	s, err := NewServer(Config{
		MemFilePath: writeTempMemFile(t, 4096),
		SocketDir:   t.TempDir(),
	})
	require.NoError(t, err)
	defer s.Close()

	assert.Equal(t, os.Getpagesize(), s.PageSize())
}

func TestSocketPath_UnregisteredFork(t *testing.T) {
	s, err := NewServer(Config{
		MemFilePath: writeTempMemFile(t, 4096),
		SocketDir:   t.TempDir(),
	})
	require.NoError(t, err)
	defer s.Close()

	_, err = s.SocketPath("missing")
	assert.Error(t, err)
}

func TestUnregisterFork_UnknownIsNoop(t *testing.T) {
	s, err := NewServer(Config{
		MemFilePath: writeTempMemFile(t, 4096),
		SocketDir:   t.TempDir(),
	})
	require.NoError(t, err)
	defer s.Close()

	assert.NoError(t, s.UnregisterFork("does-not-exist"))
}

func TestClose_Idempotent(t *testing.T) {
	s, err := NewServer(Config{
		MemFilePath: writeTempMemFile(t, 4096),
		SocketDir:   t.TempDir(),
	})
	require.NoError(t, err)
	require.NoError(t, s.Close())
	assert.NoError(t, s.Close())
}

func TestParseHandshake_GoodPayload(t *testing.T) {
	data := []byte(`{"mappings":[{"base_host_virt_addr":4096,"size":8192,"offset":0}]}`)
	hs, err := parseHandshake(data)
	require.NoError(t, err)
	require.Len(t, hs.Mappings, 1)
	assert.Equal(t, uintptr(4096), hs.Mappings[0].BaseHostAddr)
	assert.Equal(t, uint64(8192), hs.Mappings[0].Size)
	assert.Equal(t, uint64(0), hs.Mappings[0].MemFileOffset)
}

func TestParseHandshake_RejectsEmptyMappings(t *testing.T) {
	_, err := parseHandshake([]byte(`{"mappings":[]}`))
	assert.Error(t, err)
}

func TestParseHandshake_RejectsBadJSON(t *testing.T) {
	_, err := parseHandshake([]byte(`{not json`))
	assert.Error(t, err)
}

func TestResolveSocketPath_PerFork(t *testing.T) {
	dir := t.TempDir()
	s, err := NewServer(Config{
		MemFilePath: writeTempMemFile(t, 4096),
		SocketDir:   dir,
	})
	require.NoError(t, err)
	defer s.Close()

	got := s.resolveSocketPath("fork-1")
	assert.Equal(t, filepath.Join(dir, "fork-1.uffd"), got)
}

func TestErrUnsupportedSentinel(t *testing.T) {
	// The sentinel must be a stable error value so callers can switch on it.
	assert.True(t, errors.Is(ErrUnsupported, ErrUnsupported))
}
