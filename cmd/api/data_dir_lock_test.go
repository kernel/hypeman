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

func lockTestFile(t *testing.T, path string) {
	t.Helper()
	f, err := os.OpenFile(filepath.Join(path, dataDirLockName), os.O_RDWR, 0)
	require.NoError(t, err)
	t.Cleanup(func() { f.Close() })
	require.NoError(t, unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB))
}

func TestServerRefusesSecondWriterBeforeStartup(t *testing.T) {
	if os.Getenv(lockTestChild) == "1" {
		require.ErrorIs(t, run(), unix.EWOULDBLOCK)
		return
	}

	path := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(path, dataDirLockName), nil, 0600))
	lockTestFile(t, path)
	alias := filepath.Join(t.TempDir(), "same-data")
	require.NoError(t, os.Symlink(path, alias))
	runLockTestChild(t, alias)
	entries, err := os.ReadDir(path)
	require.NoError(t, err)
	require.Len(t, entries, 1, "the second API must not initialize or recover anything")
	require.Equal(t, dataDirLockName, entries[0].Name())
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
	info, err := os.Stat(filepath.Join(path, dataDirLockName))
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0600), info.Mode())
	lockTestFile(t, path)
}

func TestDataDirLockDoesNotSurviveExec(t *testing.T) {
	switch os.Getenv(lockTestChild) {
	case "1":
		require.NoError(t, lockDataDir(os.Getenv("DATA_DIR")))
		t.Setenv(lockTestChild, "after-exec")
		require.NoError(t, unix.Exec(os.Args[0], os.Args, os.Environ()))
	case "after-exec":
		lockTestFile(t, os.Getenv("DATA_DIR"))
	default:
		runLockTestChild(t, filepath.Join(t.TempDir(), "data"))
	}
}

func TestDataDirLockRejectsPublicLockFile(t *testing.T) {
	dir := t.TempDir()
	name := filepath.Join(dir, dataDirLockName)
	require.NoError(t, os.WriteFile(name, nil, 0600))
	require.NoError(t, os.Chmod(name, 0644))
	require.ErrorContains(t, lockDataDir(dir), "private regular file")
}

func TestDataDirLockRejectsSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(t.TempDir(), "target")
	require.NoError(t, os.WriteFile(target, nil, 0600))
	require.NoError(t, os.Symlink(target, filepath.Join(dir, dataDirLockName)))
	require.ErrorIs(t, lockDataDir(dir), unix.ELOOP)
}
