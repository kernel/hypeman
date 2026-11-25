package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

const lockTestChild = "HYPEMAN_TEST_LOCKED_STARTUP"

func runLockTestChild(t *testing.T, dataDir string) {
	t.Helper()
	cfg := filepath.Join(t.TempDir(), "config.yaml")
	require.NoError(t, os.WriteFile(cfg, []byte("jwt_secret: local-test-secret-for-data-dir-lock\n"), 0600))
	cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^"+t.Name()+"$", "-test.timeout=30s")
	cmd.Env = []string{lockTestChild + "=1", "CONFIG_PATH=" + cfg, "DATA_DIR=" + dataDir}
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "%s", out)
}

func lockTestDir(t *testing.T, path string) {
	t.Helper()
	dir, err := os.Open(path)
	require.NoError(t, err)
	t.Cleanup(func() { dir.Close() })
	require.NoError(t, unix.Flock(int(dir.Fd()), unix.LOCK_EX|unix.LOCK_NB))
}

func TestServerRefusesSecondWriterBeforeStartup(t *testing.T) {
	if os.Getenv(lockTestChild) == "1" {
		require.ErrorIs(t, run(), unix.EWOULDBLOCK)
		return
	}

	path := t.TempDir()
	lockTestDir(t, path)
	alias := filepath.Join(t.TempDir(), "same-data")
	require.NoError(t, os.Symlink(path, alias))
	runLockTestChild(t, alias)
	entries, err := os.ReadDir(path)
	require.NoError(t, err)
	require.Empty(t, entries, "the second API must not initialize or recover anything")
}

func TestServerKeepsStorageDirectoryLockedUntilExit(t *testing.T) {
	if os.Getenv(lockTestChild) == "1" {
		require.ErrorIs(t, run(), unix.ENOTDIR)
		runtime.GC()
		require.ErrorIs(t, lockDataDir(filepath.Clean(os.Getenv("DATA_DIR"))), unix.EWOULDBLOCK)
		return
	}

	path := t.TempDir()
	// Fail storage initialization after locking, before the network starts.
	require.NoError(t, os.WriteFile(filepath.Join(path, "system"), nil, 0600))
	link := filepath.Join(path, "link")
	require.NoError(t, os.Symlink(t.TempDir(), link))
	runLockTestChild(t, link+"/..") // Preserve the uncleaned path in the child's configuration.
	lockTestDir(t, path)
}
