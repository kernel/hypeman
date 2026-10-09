package builds

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSharedBuildSourceStagingAndRecoveryVerification(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "source.tar.gz")
	hash, err := stageBuildSource(ctx, strings.NewReader("source"), path, maxBuildSourceBytes)
	require.NoError(t, err)
	info, err := os.Stat(path)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0600), info.Mode().Perm())
	require.NoError(t, verifyBuildSource(ctx, path, hash))
	_, err = stageBuildSource(ctx, strings.NewReader("replacement"), path, maxBuildSourceBytes)
	require.Error(t, err, "exclusive staging must preserve existing source")
	require.NoError(t, verifyBuildSource(ctx, path, hash))
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	require.ErrorIs(t, verifyBuildSource(canceled, path, hash), context.Canceled)
	require.NoError(t, os.WriteFile(path, []byte("changed"), 0600))
	require.ErrorIs(t, verifyBuildSource(ctx, path, hash), ErrSourceHashMismatch)
	require.NoError(t, os.Remove(path))
	require.Error(t, verifyBuildSource(ctx, path, hash))
	target := filepath.Join(filepath.Dir(path), "target")
	require.NoError(t, os.WriteFile(target, []byte("source"), 0600))
	require.NoError(t, os.Symlink(target, path))
	require.ErrorContains(t, verifyBuildSource(ctx, path, hash), "invalid staged source")
}
