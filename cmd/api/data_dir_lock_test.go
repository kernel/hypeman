package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestServerRefusesSecondWriterBeforeStartup(t *testing.T) {
	const subprocess = "HYPEMAN_TEST_LOCKED_STARTUP"
	if os.Getenv(subprocess) == "1" {
		require.ErrorIs(t, run(), syscall.EWOULDBLOCK)
		return
	}

	path := filepath.Join(t.TempDir(), "data")
	owner, err := lockDataDir(path)
	require.NoError(t, err)
	t.Cleanup(func() { owner.Close() })

	alias := filepath.Join(t.TempDir(), "same-data")
	require.NoError(t, os.Symlink(path, alias))
	cfg := filepath.Join(t.TempDir(), "config.yaml")
	require.NoError(t, os.WriteFile(cfg, []byte("data_dir: "+alias+"\njwt_secret: local-test-secret-for-data-dir-lock\n"), 0600))
	cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestServerRefusesSecondWriterBeforeStartup$")
	cmd.Env = append(os.Environ(), subprocess+"=1", "CONFIG_PATH="+cfg, "DATA_DIR="+alias)
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "%s", out)
	entries, err := os.ReadDir(path)
	require.NoError(t, err)
	require.Empty(t, entries, "the second API must not initialize or recover anything")

	require.NoError(t, owner.Close())
	next, err := lockDataDir(alias)
	require.NoError(t, err)
	require.NoError(t, next.Close())
}
