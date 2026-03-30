package instances

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kernel/hypeman/lib/hypervisor"
	"github.com/kernel/hypeman/lib/images"
	"github.com/kernel/hypeman/lib/paths"
	"github.com/stretchr/testify/require"
)

func TestCreateInstanceClearsRetentionStateBeforeMetadataSave(t *testing.T) {
	mgr, tmpDir := setupTestManager(t)
	p := paths.New(tmpDir)

	const digest = "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
	const imageName = "docker.io/library/alpine:latest"
	seedLocalReadyImage(t, p, imageName, digest)
	writeImageRetentionState(t, p, "docker.io/library/alpine", digest, time.Date(2026, 3, 20, 0, 0, 0, 0, time.UTC))

	recorder := &stubImageUsageRecorder{
		statePath: p.ImageRetentionState("docker.io/library/alpine", trimDigestPrefix(digest)),
	}
	mgr.SetImageUsageRecorder(recorder)

	_, err := mgr.CreateInstance(context.Background(), CreateInstanceRequest{
		Name:       "retention-test",
		Image:      imageName,
		Hypervisor: hypervisor.Type("missing-hypervisor"),
	})
	require.Error(t, err)

	_, err = os.Stat(p.ImageRetentionState("docker.io/library/alpine", trimDigestPrefix(digest)))
	require.True(t, os.IsNotExist(err))

	metaFiles, err := mgr.listMetadataFiles()
	require.NoError(t, err)
	require.Len(t, metaFiles, 0)
	require.Equal(t, 1, recorder.calls)
}

func seedLocalReadyImage(t *testing.T, p *paths.Paths, imageName, digest string) {
	t.Helper()

	ref, err := images.ParseNormalizedRef(imageName)
	require.NoError(t, err)

	digestHex := trimDigestPrefix(digest)
	require.NoError(t, os.MkdirAll(p.ImageDigestDir(ref.Repository(), digestHex), 0o755))
	require.NoError(t, os.WriteFile(p.ImageDigestPath(ref.Repository(), digestHex), []byte("rootfs"), 0o644))

	meta := map[string]any{
		"name":       ref.String(),
		"digest":     digest,
		"status":     images.StatusReady,
		"created_at": time.Date(2026, 3, 20, 0, 0, 0, 0, time.UTC),
	}
	content, err := json.MarshalIndent(meta, "", "  ")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(p.ImageMetadata(ref.Repository(), digestHex), content, 0o644))
	require.NoError(t, os.MkdirAll(p.ImageRepositoryDir(ref.Repository()), 0o755))
	require.NoError(t, os.Symlink(digestHex, p.ImageTagSymlink(ref.Repository(), ref.Tag())))
}

func writeImageRetentionState(t *testing.T, p *paths.Paths, repository, digest string, unusedSince time.Time) {
	t.Helper()
	statePath := p.ImageRetentionState(repository, trimDigestPrefix(digest))
	require.NoError(t, os.MkdirAll(filepath.Dir(statePath), 0o755))

	content, err := json.MarshalIndent(map[string]any{
		"repository":   repository,
		"digest":       digest,
		"unused_since": unusedSince,
	}, "", "  ")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(statePath, content, 0o644))
}

func trimDigestPrefix(digest string) string {
	return digest[len("sha256:"):]
}

type stubImageUsageRecorder struct {
	statePath string
	calls     int
}

func (s *stubImageUsageRecorder) MarkUsed(ctx context.Context, imageName, digest string) error {
	s.calls++
	return os.Remove(s.statePath)
}
