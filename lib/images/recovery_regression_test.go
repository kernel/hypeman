package images

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kernel/hypeman/lib/paths"
	"github.com/kernel/hypeman/lib/queue"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	recoveryFixtureDir       = "testdata/recover-interrupted-build-panic"
	recoveryFixtureRepo      = "docker.io/library/hypeman-test"
	recoveryFixtureDigest    = "sha256:073e2a02f0df492def76940a909b6b79b896fc8907cceeb03452b250697d98fa"
	recoveryFixtureTag       = "073e2a02f0df492def76940a909b6b79b896fc8907cceeb03452b250697d98fa"
	recoveryFixtureDigestHex = "073e2a02f0df492def76940a909b6b79b896fc8907cceeb03452b250697d98fa"
)

func TestComposeRootfsCapturedFixtureReturnsErrorInsteadOfPanicking(t *testing.T) {
	dataDir := copyRecoveryFixture(t)

	client, err := newOCIClient(filepath.Join(dataDir, "system", "oci-cache"))
	require.NoError(t, err)

	bundle, err := client.extractOCIImageBundle(recoveryFixtureTag)
	require.NoError(t, err)

	var composeErr error
	require.NotPanics(t, func() {
		composeErr = client.composeRootfs(context.Background(), filepath.Join(t.TempDir(), "rootfs"), recoveryFixtureTag, bundle.Model)
	})

	require.Error(t, composeErr)
	assert.Contains(t, composeErr.Error(), "config rootfs.diff_ids has 0 entries but manifest has 1 layers")
}

func TestRecoverInterruptedBuildsCapturedFixtureMarksBuildFailed(t *testing.T) {
	dataDir := copyRecoveryFixture(t)
	p := paths.New(dataDir)

	client, err := newOCIClient(p.SystemOCICache())
	require.NoError(t, err)

	m := &manager{
		paths:            p,
		ociClient:        client,
		queue:            queue.New(1),
		readySubscribers: make(map[string][]chan StatusEvent),
	}

	m.RecoverInterruptedBuilds()

	require.Eventually(t, func() bool {
		meta, err := readMetadata(p, recoveryFixtureRepo, recoveryFixtureDigestHex)
		if err != nil || meta.Error == nil {
			return false
		}
		return meta.Status == StatusFailed && m.queue.QueueLength() == 0
	}, 5*time.Second, 20*time.Millisecond)

	meta, err := readMetadata(p, recoveryFixtureRepo, recoveryFixtureDigestHex)
	require.NoError(t, err)
	require.NotNil(t, meta.Error)
	assert.Equal(t, recoveryFixtureDigest, meta.Digest)
	assert.Equal(t, StatusFailed, meta.Status)
	assert.Contains(t, *meta.Error, "config rootfs.diff_ids has 0 entries but manifest has 1 layers")
}

func copyRecoveryFixture(t *testing.T) string {
	t.Helper()

	dst := t.TempDir()
	copyTree(t, recoveryFixtureDir, dst)
	return dst
}

func copyTree(t *testing.T, src, dst string) {
	t.Helper()

	entries, err := os.ReadDir(src)
	require.NoError(t, err)

	for _, entry := range entries {
		srcPath := filepath.Join(src, entry.Name())
		dstPath := filepath.Join(dst, entry.Name())

		info, err := entry.Info()
		require.NoError(t, err)

		if entry.IsDir() {
			require.NoError(t, os.MkdirAll(dstPath, info.Mode().Perm()))
			copyTree(t, srcPath, dstPath)
			continue
		}

		data, err := os.ReadFile(srcPath)
		require.NoError(t, err)
		require.NoError(t, os.MkdirAll(filepath.Dir(dstPath), 0755))
		require.NoError(t, os.WriteFile(dstPath, data, info.Mode().Perm()))
	}
}
