package builds

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/kernel/hypeman/lib/paths"
	"github.com/kernel/hypeman/lib/volumes"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// setupCacheVolumeManager returns a cache volume manager backed by mock
// volumes, plus the mock itself and a settable clock.
func setupCacheVolumeManager(t *testing.T, config CacheVolumeConfig) (*cacheVolumeManager, *mockVolumeManager, *time.Time, string) {
	t.Helper()

	tempDir, err := os.MkdirTemp("", "cache-volumes-test-*")
	require.NoError(t, err)

	volumeMgr := newMockVolumeManager()
	// Honor custom IDs and sizes like the real volume manager.
	volumeMgr.createFunc = func(ctx context.Context, req volumes.CreateVolumeRequest) (*volumes.Volume, error) {
		id := req.Name
		if req.Id != nil && *req.Id != "" {
			id = *req.Id
		}
		if _, ok := volumeMgr.volumes[id]; ok {
			return nil, volumes.ErrAlreadyExists
		}
		vol := &volumes.Volume{
			Id:        id,
			Name:      req.Name,
			SizeGb:    req.SizeGb,
			CreatedAt: time.Now(),
		}
		volumeMgr.volumes[id] = vol
		return vol, nil
	}

	now := time.Now()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	mgr := newCacheVolumeManager(config, paths.New(tempDir), volumeMgr, logger)
	mgr.now = func() time.Time { return now }

	return mgr, volumeMgr, &now, tempDir
}

func TestCacheVolumeID(t *testing.T) {
	idA := cacheVolumeID("tenant-a")

	assert.Equal(t, idA, cacheVolumeID("tenant-a"), "deterministic")
	assert.NotEqual(t, idA, cacheVolumeID("tenant-b"), "distinct per scope")
	assert.Contains(t, idA, "build-cache-")
	assert.Len(t, idA, len("build-cache-")+64, "build-cache-<sha256 hex>")
}

func TestEnsureCacheVolume_CreatesWithFixedSize(t *testing.T) {
	mgr, volumeMgr, _, tempDir := setupCacheVolumeManager(t, CacheVolumeConfig{Enabled: true, SizeGB: 42})
	defer os.RemoveAll(tempDir)

	volID, err := mgr.ensureCacheVolume(context.Background(), "tenant-a")

	require.NoError(t, err)
	assert.Equal(t, cacheVolumeID("tenant-a"), volID)
	vol, err := volumeMgr.GetVolume(context.Background(), volID)
	require.NoError(t, err)
	assert.Equal(t, 42, vol.SizeGb)
}

func TestEnsureCacheVolume_DefaultSize(t *testing.T) {
	mgr, volumeMgr, _, tempDir := setupCacheVolumeManager(t, CacheVolumeConfig{Enabled: true})
	defer os.RemoveAll(tempDir)

	volID, err := mgr.ensureCacheVolume(context.Background(), "tenant-a")

	require.NoError(t, err)
	vol, err := volumeMgr.GetVolume(context.Background(), volID)
	require.NoError(t, err)
	assert.Equal(t, DefaultCacheVolumeSizeGB, vol.SizeGb)
}

func TestEnsureCacheVolume_ReusesExisting(t *testing.T) {
	mgr, volumeMgr, _, tempDir := setupCacheVolumeManager(t, CacheVolumeConfig{Enabled: true, SizeGB: 42})
	defer os.RemoveAll(tempDir)

	volID1, err := mgr.ensureCacheVolume(context.Background(), "tenant-a")
	require.NoError(t, err)
	volID2, err := mgr.ensureCacheVolume(context.Background(), "tenant-a")
	require.NoError(t, err)

	assert.Equal(t, volID1, volID2)
	assert.Equal(t, 1, volumeMgr.createCallCount)
}

func TestLockScope_SerializesSameScope(t *testing.T) {
	mgr, _, _, tempDir := setupCacheVolumeManager(t, CacheVolumeConfig{Enabled: true})
	defer os.RemoveAll(tempDir)

	unlock := mgr.lockScope("tenant-a")

	acquired := make(chan struct{})
	go func() {
		unlock2 := mgr.lockScope("tenant-a")
		close(acquired)
		unlock2()
	}()

	select {
	case <-acquired:
		t.Fatal("second lock on same scope acquired while first was held")
	case <-time.After(50 * time.Millisecond):
	}

	unlock()
	select {
	case <-acquired:
	case <-time.After(2 * time.Second):
		t.Fatal("second lock not acquired after release")
	}
}

func TestLockScope_IndependentAcrossScopes(t *testing.T) {
	mgr, _, _, tempDir := setupCacheVolumeManager(t, CacheVolumeConfig{Enabled: true})
	defer os.RemoveAll(tempDir)

	unlockA := mgr.lockScope("tenant-a")
	defer unlockA()

	done := make(chan struct{})
	go func() {
		unlockB := mgr.lockScope("tenant-b")
		unlockB()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("lock on a different scope blocked")
	}
}

func TestReap_IdleTTLDeletesOldVolumes(t *testing.T) {
	mgr, volumeMgr, now, tempDir := setupCacheVolumeManager(t, CacheVolumeConfig{
		Enabled: true,
		SizeGB:  10,
		IdleTTL: time.Hour,
	})
	defer os.RemoveAll(tempDir)

	ctx := context.Background()
	oldVol, err := mgr.ensureCacheVolume(ctx, "tenant-old")
	require.NoError(t, err)

	// Age the volume beyond the TTL, then create a fresh one.
	*now = now.Add(-2 * time.Hour)
	mgr.touchLastUsed(oldVol)
	*now = time.Now()

	freshVol, err := mgr.ensureCacheVolume(ctx, "tenant-fresh")
	require.NoError(t, err)

	mgr.reap(ctx)

	_, err = volumeMgr.GetVolume(ctx, oldVol)
	assert.ErrorIs(t, err, volumes.ErrNotFound, "idle-past-TTL volume evicted")

	_, err = volumeMgr.GetVolume(ctx, freshVol)
	assert.NoError(t, err, "fresh volume kept")
}

func TestReap_NeverEvictsAttachedVolume(t *testing.T) {
	mgr, volumeMgr, now, tempDir := setupCacheVolumeManager(t, CacheVolumeConfig{
		Enabled: true,
		SizeGB:  10,
		IdleTTL: time.Hour,
	})
	defer os.RemoveAll(tempDir)

	ctx := context.Background()
	volID, err := mgr.ensureCacheVolume(ctx, "tenant-a")
	require.NoError(t, err)

	*now = now.Add(-2 * time.Hour)
	mgr.touchLastUsed(volID)
	*now = time.Now()

	// Simulate an active attachment (e.g. a build in progress).
	volumeMgr.volumes[volID].Attachments = []volumes.Attachment{
		{InstanceID: "inst-1", MountPath: "/var/lib/buildkit"},
	}

	mgr.reap(ctx)

	_, err = volumeMgr.GetVolume(ctx, volID)
	assert.NoError(t, err, "attached volume must never be evicted")
}

func TestReap_NeverEvictsInUseVolume(t *testing.T) {
	mgr, volumeMgr, now, tempDir := setupCacheVolumeManager(t, CacheVolumeConfig{
		Enabled: true,
		SizeGB:  10,
		IdleTTL: time.Hour,
	})
	defer os.RemoveAll(tempDir)

	ctx := context.Background()
	volID, err := mgr.ensureCacheVolume(ctx, "tenant-a")
	require.NoError(t, err)

	*now = now.Add(-2 * time.Hour)
	mgr.touchLastUsed(volID)
	*now = time.Now()

	release := mgr.acquireVolume(volID)
	mgr.reap(ctx)

	_, err = volumeMgr.GetVolume(ctx, volID)
	assert.NoError(t, err, "in-use volume must never be evicted")

	release()
	mgr.reap(ctx)

	_, err = volumeMgr.GetVolume(ctx, volID)
	assert.ErrorIs(t, err, volumes.ErrNotFound, "volume evicted once released")
}

func TestReap_EnforcesMaxVolumesLRU(t *testing.T) {
	mgr, volumeMgr, now, tempDir := setupCacheVolumeManager(t, CacheVolumeConfig{
		Enabled:    true,
		SizeGB:     10,
		IdleTTL:    100 * time.Hour, // TTL not in play
		MaxVolumes: 2,
	})
	defer os.RemoveAll(tempDir)

	ctx := context.Background()
	base := *now
	volIDs := make([]string, 3)
	for i, scope := range []string{"tenant-1", "tenant-2", "tenant-3"} {
		volID, err := mgr.ensureCacheVolume(ctx, scope)
		require.NoError(t, err)
		// tenant-1 is least recently used, tenant-3 most recent
		*now = base.Add(time.Duration(i-3) * time.Hour)
		mgr.touchLastUsed(volID)
		volIDs[i] = volID
	}
	*now = base

	mgr.reap(ctx)

	_, err := volumeMgr.GetVolume(ctx, volIDs[0])
	assert.ErrorIs(t, err, volumes.ErrNotFound, "oldest volume evicted first")
	for _, id := range volIDs[1:] {
		_, err := volumeMgr.GetVolume(ctx, id)
		assert.NoError(t, err, "newer volumes kept")
	}
}

func TestReap_EnforcesMaxBytesLRU(t *testing.T) {
	mgr, volumeMgr, now, tempDir := setupCacheVolumeManager(t, CacheVolumeConfig{
		Enabled:  true,
		SizeGB:   10,
		IdleTTL:  100 * time.Hour,
		MaxBytes: 20 * 1024 * 1024 * 1024, // room for 2 of the 3 volumes
	})
	defer os.RemoveAll(tempDir)

	ctx := context.Background()
	base := *now
	volIDs := make([]string, 3)
	for i, scope := range []string{"tenant-1", "tenant-2", "tenant-3"} {
		volID, err := mgr.ensureCacheVolume(ctx, scope)
		require.NoError(t, err)
		*now = base.Add(time.Duration(i-3) * time.Hour)
		mgr.touchLastUsed(volID)
		volIDs[i] = volID
	}
	*now = base

	mgr.reap(ctx)

	_, err := volumeMgr.GetVolume(ctx, volIDs[0])
	assert.ErrorIs(t, err, volumes.ErrNotFound, "oldest volume evicted to satisfy byte limit")
	for _, id := range volIDs[1:] {
		_, err := volumeMgr.GetVolume(ctx, id)
		assert.NoError(t, err, "newer volumes kept")
	}
}

func TestReap_LimitsSkipAttachedVolumes(t *testing.T) {
	mgr, volumeMgr, now, tempDir := setupCacheVolumeManager(t, CacheVolumeConfig{
		Enabled:    true,
		SizeGB:     10,
		IdleTTL:    100 * time.Hour,
		MaxVolumes: 1,
	})
	defer os.RemoveAll(tempDir)

	ctx := context.Background()
	base := *now

	attachedVol, err := mgr.ensureCacheVolume(ctx, "tenant-attached")
	require.NoError(t, err)
	*now = base.Add(-3 * time.Hour)
	mgr.touchLastUsed(attachedVol)
	volumeMgr.volumes[attachedVol].Attachments = []volumes.Attachment{
		{InstanceID: "inst-1", MountPath: "/var/lib/buildkit"},
	}

	idleVol, err := mgr.ensureCacheVolume(ctx, "tenant-idle")
	require.NoError(t, err)
	*now = base.Add(-time.Hour)
	mgr.touchLastUsed(idleVol)
	*now = base

	mgr.reap(ctx)

	_, err = volumeMgr.GetVolume(ctx, attachedVol)
	assert.NoError(t, err, "attached volume kept even over the count limit")
	_, err = volumeMgr.GetVolume(ctx, idleVol)
	assert.ErrorIs(t, err, volumes.ErrNotFound, "idle volume evicted instead")
}

func TestCacheVolumeStatePersistence(t *testing.T) {
	config := CacheVolumeConfig{Enabled: true, SizeGB: 10}
	mgr, _, now, tempDir := setupCacheVolumeManager(t, config)
	defer os.RemoveAll(tempDir)

	ctx := context.Background()
	volID, err := mgr.ensureCacheVolume(ctx, "tenant-a")
	require.NoError(t, err)

	*now = now.Add(-time.Hour)
	mgr.touchLastUsed(volID)
	lastUsed := *now

	// A new manager over the same data dir sees the same last-used metadata.
	mgr2, _, _, _ := setupCacheVolumeManager(t, config)
	mgr2.paths = mgr.paths
	mgr2.mu.Lock()
	mgr2.loadLocked()
	got := mgr2.lastUsed[volID]
	mgr2.mu.Unlock()

	assert.True(t, got.Equal(lastUsed), "last-used persisted across restarts: got %v want %v", got, lastUsed)
}

func TestNewManager_CacheVolumesWiring(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "builds-cache-wiring-*")
	require.NoError(t, err)
	defer os.RemoveAll(tempDir)

	p := paths.New(tempDir)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	config := Config{
		MaxConcurrentBuilds: 1,
		RegistryURL:         "localhost:5000",
		RegistrySecret:      "test-secret",
	}

	// Disabled by default: no cache volume manager, no reaper.
	disabled, err := NewManager(p, config, newMockInstanceManager(), newMockVolumeManager(), newMockImageManager(), &mockSecretProvider{}, logger, nil)
	require.NoError(t, err)
	assert.Nil(t, disabled.(*manager).cacheVolumes)

	// Enabled: cache volume manager is constructed.
	config.Cache.Enabled = true
	enabled, err := NewManager(p, config, newMockInstanceManager(), newMockVolumeManager(), newMockImageManager(), &mockSecretProvider{}, logger, nil)
	require.NoError(t, err)
	assert.NotNil(t, enabled.(*manager).cacheVolumes)
}

// TestCacheVolumeID_TenantIsolation verifies scopes never share a volume.
func TestCacheVolumeID_TenantIsolation(t *testing.T) {
	scopes := []string{"tenant-a", "tenant-b", "tenant-c", "my-team"}
	seen := make(map[string]string)
	for _, s := range scopes {
		id := cacheVolumeID(s)
		if other, dup := seen[id]; dup {
			t.Fatalf("scopes %q and %q share volume ID %s", other, s, id)
		}
		seen[id] = s
	}
}

// TestLockScope_ConcurrentEnsure verifies concurrent ensures for the same
// scope create exactly one volume when serialized through lockScope.
func TestLockScope_ConcurrentEnsure(t *testing.T) {
	mgr, volumeMgr, _, tempDir := setupCacheVolumeManager(t, CacheVolumeConfig{Enabled: true, SizeGB: 10})
	defer os.RemoveAll(tempDir)

	ctx := context.Background()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			unlock := mgr.lockScope("tenant-a")
			defer unlock()
			_, err := mgr.ensureCacheVolume(ctx, "tenant-a")
			assert.NoError(t, err)
		}()
	}
	wg.Wait()

	assert.Equal(t, 1, volumeMgr.createCallCount, fmt.Sprintf("expected 1 create, got %d", volumeMgr.createCallCount))
}
