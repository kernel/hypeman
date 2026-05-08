package uffd

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHotPageList_SnapshotSortsAndDedups(t *testing.T) {
	var l HotPageList
	l.Add(HotPage{Region: 1, PageOffset: 8192})
	l.Add(HotPage{Region: 0, PageOffset: 4096})
	l.Add(HotPage{Region: 0, PageOffset: 4096}) // duplicate
	l.Add(HotPage{Region: 0, PageOffset: 0})

	got := l.Snapshot()
	want := []HotPage{
		{Region: 0, PageOffset: 0},
		{Region: 0, PageOffset: 4096},
		{Region: 1, PageOffset: 8192},
	}
	assert.Equal(t, want, got)
}

func TestHotPageList_SaveLoadRoundTrip(t *testing.T) {
	var l HotPageList
	l.Add(HotPage{Region: 0, PageOffset: 0})
	l.Add(HotPage{Region: 0, PageOffset: 4096})
	l.Add(HotPage{Region: 2, PageOffset: 1 << 20})

	path := filepath.Join(t.TempDir(), "hot.bin")
	require.NoError(t, l.Save(path))

	got, err := LoadHotPageList(path)
	require.NoError(t, err)
	assert.Equal(t, l.Snapshot(), got.Snapshot())
}

func TestLoadHotPageList_MissingReturnsEmpty(t *testing.T) {
	got, err := LoadHotPageList(filepath.Join(t.TempDir(), "absent.bin"))
	require.NoError(t, err)
	assert.Equal(t, 0, got.Len())
}

func TestLoadHotPageList_BadMagic(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bad.bin")
	require.NoError(t, writeFile(path, []byte("XXXX\x00")))
	_, err := LoadHotPageList(path)
	assert.Error(t, err)
}

func TestLoadHotPageList_TruncatedAtEntry(t *testing.T) {
	path := filepath.Join(t.TempDir(), "trunc.bin")
	// magic + count=2 + only one entry
	data := append([]byte("HPL1"), 0x02, 0x00, 0x00) // count=2, region=0, offset=0
	require.NoError(t, writeFile(path, data))
	_, err := LoadHotPageList(path)
	assert.Error(t, err)
}

func writeFile(path string, data []byte) error {
	return os.WriteFile(path, data, 0o600)
}
