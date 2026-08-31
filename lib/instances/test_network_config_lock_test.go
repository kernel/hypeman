package instances

import (
	"path/filepath"
	"syscall"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestTryWithTestFileLock(t *testing.T) {
	lockPath := filepath.Join(t.TempDir(), "test.lock")

	called := false
	acquired, err := tryWithTestFileLock(lockPath, func() error {
		called = true
		return nil
	})
	require.NoError(t, err)
	require.True(t, acquired)
	require.True(t, called)

	heldLock, err := openTestLockFile(lockPath)
	require.NoError(t, err)
	defer heldLock.Close()
	require.NoError(t, syscall.Flock(int(heldLock.Fd()), syscall.LOCK_EX))
	defer syscall.Flock(int(heldLock.Fd()), syscall.LOCK_UN)

	called = false
	acquired, err = tryWithTestFileLock(lockPath, func() error {
		called = true
		return nil
	})
	require.NoError(t, err)
	require.False(t, acquired)
	require.False(t, called)
}

func TestParseIPTablesAppendRule(t *testing.T) {
	line := `-A FORWARD -i hm1234 -o bond0 -m comment --comment "hypeman-fwd-out-hm1234" -j ACCEPT`
	args, comment, ok := parseIPTablesAppendRule("filter", line)
	require.True(t, ok)
	require.Equal(t, "hypeman-fwd-out-hm1234", comment)
	require.Equal(t, []string{
		"-t", "filter", "-D", "FORWARD", "-i", "hm1234", "-o", "bond0",
		"-m", "comment", "--comment", "hypeman-fwd-out-hm1234", "-j", "ACCEPT",
	}, args)
}
