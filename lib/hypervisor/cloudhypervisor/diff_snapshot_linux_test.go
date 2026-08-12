//go:build linux

package cloudhypervisor

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

func TestMergeCloudHypervisorDiff(t *testing.T) {
	dir := t.TempDir()
	const size = 4 << 20
	base := bytes.Repeat([]byte{0x7b}, size)
	basePath := filepath.Join(dir, cloudHypervisorMemoryFile)
	diffPath := filepath.Join(dir, cloudHypervisorDiffMemoryFile)
	require.NoError(t, os.WriteFile(basePath, base, 0600))

	diff, err := os.OpenFile(diffPath, os.O_CREATE|os.O_RDWR, 0600)
	require.NoError(t, err)
	require.NoError(t, diff.Truncate(size))
	changed := bytes.Repeat([]byte{0x2a}, 4096)
	zeroed := make([]byte, 4096)
	_, err = diff.WriteAt(changed, 64*4096)
	require.NoError(t, err)
	_, err = diff.WriteAt(zeroed, 512*4096)
	require.NoError(t, err)
	require.NoError(t, diff.Sync())
	require.NoError(t, diff.Close())

	stats, err := mergeCloudHypervisorDiff(dir)
	if errors.Is(err, unix.EINVAL) || errors.Is(err, unix.ENOTSUP) || errors.Is(err, unix.EOPNOTSUPP) {
		t.Skipf("filesystem does not support sparse extent discovery: %v", err)
	}
	require.NoError(t, err)
	assert.GreaterOrEqual(t, stats.DeltaBytes, int64(2*4096))
	assert.Equal(t, stats.DeltaBytes, stats.ReflinkedBytes+stats.CopiedBytes)
	assert.NoFileExists(t, diffPath)

	want := append([]byte(nil), base...)
	copy(want[64*4096:], changed)
	copy(want[512*4096:], zeroed)
	got, err := os.ReadFile(basePath)
	require.NoError(t, err)
	assert.Equal(t, want, got)
}
