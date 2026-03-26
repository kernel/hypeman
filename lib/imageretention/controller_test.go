package imageretention

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kernel/hypeman/lib/images"
	"github.com/kernel/hypeman/lib/instances"
	"github.com/kernel/hypeman/lib/paths"
	snapshotstore "github.com/kernel/hypeman/lib/snapshot"
	"github.com/stretchr/testify/require"
)

func TestSweepMarksUnreferencedImageUnused(t *testing.T) {
	controller, p, _ := newTestController(t, 30*24*time.Hour)
	const digest = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	seedReadyImage(t, p, "docker.io/library/alpine:latest", digest)

	now := time.Date(2026, 3, 26, 12, 0, 0, 0, time.UTC)
	controller.now = func() time.Time { return now }

	require.NoError(t, controller.Sweep(context.Background()))

	state := readState(t, p, "docker.io/library/alpine", digest)
	require.Equal(t, now, state.UnusedSince)
}

func TestSweepReferencedInstancePreventsUnusedState(t *testing.T) {
	controller, p, _ := newTestController(t, 30*24*time.Hour)
	const digest = "sha256:2222222222222222222222222222222222222222222222222222222222222222"
	imageName := "docker.io/library/alpine:latest"
	seedReadyImage(t, p, imageName, digest)
	writeInstanceMetadata(t, p, "inst-1", instances.StoredMetadata{
		Id:    "inst-1",
		Name:  "instance-1",
		Image: imageName,
	})

	require.NoError(t, controller.Sweep(context.Background()))

	_, err := os.Stat(p.ImageRetentionState("docker.io/library/alpine", stringsTrimDigest(digest)))
	require.Error(t, err)
	require.True(t, os.IsNotExist(err))
}

func TestSweepSnapshotReferencePreventsDeletion(t *testing.T) {
	controller, p, _ := newTestController(t, 24*time.Hour)
	const digest = "sha256:3333333333333333333333333333333333333333333333333333333333333333"
	imageName := "docker.io/library/alpine:latest"
	seedReadyImage(t, p, imageName, digest)
	writeSnapshotRecord(t, p, "snap-1", instances.StoredMetadata{Image: imageName})
	writeStateAt(t, p, "docker.io/library/alpine", digest, time.Date(2026, 3, 20, 0, 0, 0, 0, time.UTC))

	controller.now = func() time.Time {
		return time.Date(2026, 3, 26, 0, 0, 0, 0, time.UTC)
	}

	require.NoError(t, controller.Sweep(context.Background()))

	_, err := os.Stat(p.ImageDigestDir("docker.io/library/alpine", stringsTrimDigest(digest)))
	require.NoError(t, err)
	_, err = os.Stat(p.ImageRetentionState("docker.io/library/alpine", stringsTrimDigest(digest)))
	require.True(t, os.IsNotExist(err))
}

func TestSweepDeletesExpiredImageDigestAndAllTags(t *testing.T) {
	controller, p, _ := newTestController(t, 24*time.Hour)
	const digest = "sha256:4444444444444444444444444444444444444444444444444444444444444444"
	seedReadyImage(t, p, "docker.io/library/alpine:latest", digest, "stable")
	writeStateAt(t, p, "docker.io/library/alpine", digest, time.Date(2026, 3, 20, 0, 0, 0, 0, time.UTC))

	controller.now = func() time.Time {
		return time.Date(2026, 3, 26, 0, 0, 0, 0, time.UTC)
	}

	require.NoError(t, controller.Sweep(context.Background()))

	_, err := os.Stat(p.ImageDigestDir("docker.io/library/alpine", stringsTrimDigest(digest)))
	require.True(t, os.IsNotExist(err))
	_, err = os.Stat(p.ImageTagSymlink("docker.io/library/alpine", "latest"))
	require.True(t, os.IsNotExist(err))
	_, err = os.Stat(p.ImageTagSymlink("docker.io/library/alpine", "stable"))
	require.True(t, os.IsNotExist(err))
}

func TestSweepProtectedImageClearsPriorState(t *testing.T) {
	controller, p, _ := newTestController(t, 24*time.Hour)
	const digest = "sha256:5555555555555555555555555555555555555555555555555555555555555555"
	imageName := "docker.io/library/alpine:latest"
	seedReadyImage(t, p, imageName, digest)
	writeStateAt(t, p, "docker.io/library/alpine", digest, time.Date(2026, 3, 20, 0, 0, 0, 0, time.UTC))
	writeInstanceMetadata(t, p, "inst-1", instances.StoredMetadata{
		Id:    "inst-1",
		Name:  "instance-1",
		Image: imageName,
	})

	require.NoError(t, controller.Sweep(context.Background()))

	_, err := os.Stat(p.ImageRetentionState("docker.io/library/alpine", stringsTrimDigest(digest)))
	require.True(t, os.IsNotExist(err))
}

func TestSweepPrunesStateForManuallyDeletedImage(t *testing.T) {
	controller, p, _ := newTestController(t, 24*time.Hour)
	const digest = "sha256:6666666666666666666666666666666666666666666666666666666666666666"
	seedReadyImage(t, p, "docker.io/library/alpine:latest", digest)
	writeStateAt(t, p, "docker.io/library/alpine", digest, time.Date(2026, 3, 20, 0, 0, 0, 0, time.UTC))
	require.NoError(t, os.RemoveAll(p.ImageDigestDir("docker.io/library/alpine", stringsTrimDigest(digest))))

	require.NoError(t, controller.Sweep(context.Background()))

	_, err := os.Stat(p.ImageRetentionState("docker.io/library/alpine", stringsTrimDigest(digest)))
	require.True(t, os.IsNotExist(err))
}

func TestSweepIgnoresNonReadyImages(t *testing.T) {
	controller, p, _ := newTestController(t, 24*time.Hour)
	const digest = "sha256:7777777777777777777777777777777777777777777777777777777777777777"
	seedImage(t, p, "docker.io/library/alpine:latest", digest, images.StatusPending)

	require.NoError(t, controller.Sweep(context.Background()))

	_, err := os.Stat(p.ImageRetentionState("docker.io/library/alpine", stringsTrimDigest(digest)))
	require.True(t, os.IsNotExist(err))
}

func TestSweepStaleReferencesDoNotBlockOtherCleanup(t *testing.T) {
	controller, p, _ := newTestController(t, 24*time.Hour)
	const staleDigest = "sha256:8888888888888888888888888888888888888888888888888888888888888888"
	const liveDigest = "sha256:9999999999999999999999999999999999999999999999999999999999999999"
	seedReadyImage(t, p, "docker.io/library/alpine:latest", liveDigest)
	writeStateAt(t, p, "docker.io/library/alpine", liveDigest, time.Date(2026, 3, 20, 0, 0, 0, 0, time.UTC))
	writeInstanceMetadata(t, p, "inst-1", instances.StoredMetadata{
		Id:    "inst-1",
		Name:  "instance-1",
		Image: "docker.io/library/busybox@" + staleDigest,
	})

	controller.now = func() time.Time {
		return time.Date(2026, 3, 26, 0, 0, 0, 0, time.UTC)
	}

	require.NoError(t, controller.Sweep(context.Background()))

	_, err := os.Stat(p.ImageDigestDir("docker.io/library/alpine", stringsTrimDigest(liveDigest)))
	require.True(t, os.IsNotExist(err))
}

func TestMarkUsedClearsState(t *testing.T) {
	controller, p, _ := newTestController(t, 24*time.Hour)
	const digest = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	seedReadyImage(t, p, "docker.io/library/alpine:latest", digest)
	writeStateAt(t, p, "docker.io/library/alpine", digest, time.Date(2026, 3, 20, 0, 0, 0, 0, time.UTC))

	require.NoError(t, controller.MarkUsed(context.Background(), "docker.io/library/alpine:latest", digest))

	_, err := os.Stat(p.ImageRetentionState("docker.io/library/alpine", stringsTrimDigest(digest)))
	require.True(t, os.IsNotExist(err))
}

func TestRunPerformsImmediateSweep(t *testing.T) {
	controller, p, _ := newTestController(t, 24*time.Hour)
	const digest = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	seedReadyImage(t, p, "docker.io/library/alpine:latest", digest)
	now := time.Date(2026, 3, 26, 12, 0, 0, 0, time.UTC)
	controller.now = func() time.Time { return now }

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = controller.Run(ctx)
	}()

	statePath := p.ImageRetentionState("docker.io/library/alpine", stringsTrimDigest(digest))
	require.Eventually(t, func() bool {
		_, err := os.Stat(statePath)
		return err == nil
	}, time.Second, 10*time.Millisecond)

	cancel()
	<-done

	state := readState(t, p, "docker.io/library/alpine", digest)
	require.Equal(t, now, state.UnusedSince)
}

func newTestController(t *testing.T, unusedFor time.Duration) (*Controller, *paths.Paths, images.Manager) {
	t.Helper()

	dataDir := t.TempDir()
	p := paths.New(dataDir)
	manager, err := images.NewManager(p, 1, nil)
	require.NoError(t, err)

	return NewController(p, manager, unusedFor, nil), p, manager
}

func seedReadyImage(t *testing.T, p *paths.Paths, imageName, digest string, extraTags ...string) {
	t.Helper()
	seedImage(t, p, imageName, digest, images.StatusReady, extraTags...)
}

func seedImage(t *testing.T, p *paths.Paths, imageName, digest, status string, extraTags ...string) {
	t.Helper()

	ref, err := images.ParseNormalizedRef(imageName)
	require.NoError(t, err)

	meta := map[string]any{
		"name":       ref.String(),
		"digest":     digest,
		"status":     status,
		"created_at": time.Date(2026, 3, 20, 0, 0, 0, 0, time.UTC),
	}

	digestHex := stringsTrimDigest(digest)
	dir := p.ImageDigestDir(ref.Repository(), digestHex)
	require.NoError(t, os.MkdirAll(dir, 0o755))
	if status == images.StatusReady {
		require.NoError(t, os.WriteFile(p.ImageDigestPath(ref.Repository(), digestHex), []byte("rootfs"), 0o644))
	}
	content, err := json.MarshalIndent(meta, "", "  ")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(p.ImageMetadata(ref.Repository(), digestHex), content, 0o644))

	if !ref.IsDigest() {
		require.NoError(t, os.MkdirAll(p.ImageRepositoryDir(ref.Repository()), 0o755))
		require.NoError(t, os.Symlink(digestHex, p.ImageTagSymlink(ref.Repository(), ref.Tag())))
	}
	for _, tag := range extraTags {
		require.NoError(t, os.Symlink(digestHex, p.ImageTagSymlink(ref.Repository(), tag)))
	}
}

func writeInstanceMetadata(t *testing.T, p *paths.Paths, id string, stored instances.StoredMetadata) {
	t.Helper()
	require.NoError(t, os.MkdirAll(p.InstanceDir(id), 0o755))
	content, err := json.MarshalIndent(stored, "", "  ")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(p.InstanceMetadata(id), content, 0o644))
}

func writeSnapshotRecord(t *testing.T, p *paths.Paths, snapshotID string, stored instances.StoredMetadata) {
	t.Helper()
	content, err := json.Marshal(stored)
	require.NoError(t, err)

	store := snapshotstore.NewStore(p)
	err = store.SaveRecord(&snapshotstore.Record{
		Snapshot: snapshotstore.Snapshot{
			Id:               snapshotID,
			SourceInstanceID: "inst-1",
			CreatedAt:        time.Date(2026, 3, 20, 0, 0, 0, 0, time.UTC),
		},
		StoredMetadata: content,
	})
	require.NoError(t, err)
}

func writeStateAt(t *testing.T, p *paths.Paths, repository, digest string, unusedSince time.Time) {
	t.Helper()
	statePath := p.ImageRetentionState(repository, stringsTrimDigest(digest))
	require.NoError(t, os.MkdirAll(filepath.Dir(statePath), 0o755))
	content, err := json.MarshalIndent(imageState{
		Repository:  repository,
		Digest:      digest,
		UnusedSince: unusedSince,
	}, "", "  ")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(statePath, content, 0o644))
}

func readState(t *testing.T, p *paths.Paths, repository, digest string) imageState {
	t.Helper()
	content, err := os.ReadFile(p.ImageRetentionState(repository, stringsTrimDigest(digest)))
	require.NoError(t, err)
	var state imageState
	require.NoError(t, json.Unmarshal(content, &state))
	return state
}

func stringsTrimDigest(digest string) string {
	return digest[len("sha256:"):]
}
