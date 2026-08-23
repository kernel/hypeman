package instances

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/kernel/hypeman/lib/paths"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestResolveCreateExpiration(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	ttl := 6 * time.Hour
	expiresAt, err := resolveCreateExpiration(CreateInstanceRequest{TTL: &ttl}, now)
	require.NoError(t, err)
	require.NotNil(t, expiresAt)
	assert.Equal(t, now.Add(6*time.Hour), *expiresAt)

	expiresAt, err = resolveCreateExpiration(CreateInstanceRequest{}, now)
	require.NoError(t, err)
	assert.Nil(t, expiresAt)

	past := now.Add(-time.Second)
	_, err = resolveCreateExpiration(CreateInstanceRequest{ExpiresAt: &past}, now)
	require.ErrorIs(t, err, ErrInvalidExpiresAt)

	zero := time.Duration(0)
	future := now.Add(time.Hour)
	err = validateCreateRequest(&CreateInstanceRequest{
		Name:      "conflicting-expiration",
		Image:     "alpine",
		TTL:       &zero,
		ExpiresAt: &future,
	})
	require.ErrorIs(t, err, ErrInvalidRequest)
}

func TestInstanceExpired(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	before := now.Add(time.Hour)
	at := now
	after := now.Add(-time.Hour)

	for _, tc := range []struct {
		name string
		meta *StoredMetadata
		want bool
	}{
		{name: "before deadline", meta: &StoredMetadata{ExpiresAt: &before}},
		{name: "at deadline", meta: &StoredMetadata{ExpiresAt: &at}, want: true},
		{name: "after deadline", meta: &StoredMetadata{ExpiresAt: &after}, want: true},
		{name: "expiration disabled", meta: &StoredMetadata{}},
		{name: "missing metadata"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, instanceExpired(tc.meta, now))
		})
	}
}

func TestTTLReaperUsesPersistedExpirationAfterRestart(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	p := paths.New(t.TempDir())
	writer := &manager{paths: p}

	save := func(id string, expiresAt *time.Time) {
		t.Helper()
		require.NoError(t, writer.ensureDirectories(id))
		require.NoError(t, writer.saveMetadata(&metadata{StoredMetadata: StoredMetadata{
			Id:        id,
			Name:      id,
			CreatedAt: now.Add(-time.Hour),
			ExpiresAt: expiresAt,
			DataDir:   p.InstanceDir(id),
		}}))
	}

	expiredAt := now.Add(-time.Minute)
	future := now.Add(time.Hour)
	save("expired", &expiredAt)
	save("not-expired", &future)
	save("no-expiration", nil)

	restarted := &manager{
		paths:           p,
		now:             func() time.Time { return now },
		lifecycleEvents: newLifecycleSubscribersWithBufferSize(defaultLifecycleEventBufferSize),
	}
	restarted.reapExpiredInstances(context.Background())

	_, err := os.Stat(p.InstanceDir("expired"))
	require.ErrorIs(t, err, os.ErrNotExist)
	for _, id := range []string{"not-expired", "no-expiration"} {
		_, err := os.Stat(p.InstanceDir(id))
		require.NoError(t, err)
	}
}

func TestReapExpiredInstanceRechecksExpirationUnderLock(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	p := paths.New(t.TempDir())
	m := &manager{
		paths:           p,
		now:             func() time.Time { return now },
		lifecycleEvents: newLifecycleSubscribersWithBufferSize(defaultLifecycleEventBufferSize),
	}
	id := "extended-before-reap"
	require.NoError(t, m.ensureDirectories(id))
	expiredAt := now.Add(-time.Minute)
	meta := &metadata{StoredMetadata: StoredMetadata{
		Id:        id,
		Name:      id,
		CreatedAt: now.Add(-time.Hour),
		ExpiresAt: &expiredAt,
		DataDir:   p.InstanceDir(id),
	}}
	require.NoError(t, m.saveMetadata(meta))
	require.True(t, instanceExpired(&meta.StoredMetadata, now))

	future := now.Add(time.Hour)
	meta.ExpiresAt = &future
	require.NoError(t, m.saveMetadata(meta))

	deleted, err := m.reapExpiredInstance(context.Background(), id)
	require.NoError(t, err)
	assert.False(t, deleted)
	_, err = os.Stat(p.InstanceDir(id))
	require.NoError(t, err)
}

func TestStartTTLReaperSweepsBeforeFirstTick(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	p := paths.New(t.TempDir())
	m := &manager{
		paths:           p,
		now:             func() time.Time { return now },
		lifecycleEvents: newLifecycleSubscribersWithBufferSize(defaultLifecycleEventBufferSize),
	}
	id := "expired-at-startup"
	expiredAt := now.Add(-time.Minute)
	require.NoError(t, m.ensureDirectories(id))
	require.NoError(t, m.saveMetadata(&metadata{StoredMetadata: StoredMetadata{
		Id:        id,
		Name:      id,
		CreatedAt: now.Add(-time.Hour),
		ExpiresAt: &expiredAt,
		DataDir:   p.InstanceDir(id),
	}}))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- m.StartTTLReaper(ctx)
	}()

	require.Eventually(t, func() bool {
		_, err := os.Stat(p.InstanceDir(id))
		return os.IsNotExist(err)
	}, time.Second, 10*time.Millisecond)
	cancel()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("ttl reaper did not stop after cancellation")
	}
}

func TestTTLReaperDeleteTimeoutDoesNotBlockSweep(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	p := paths.New(t.TempDir())
	m := &manager{
		paths:                  p,
		now:                    func() time.Time { return now },
		lifecycleEvents:        newLifecycleSubscribersWithBufferSize(defaultLifecycleEventBufferSize),
		ttlReaperDeleteTimeout: 100 * time.Millisecond,
	}
	expiredAt := now.Add(-time.Minute)
	for _, id := range []string{"a-timeout", "b-deleted"} {
		require.NoError(t, m.ensureDirectories(id))
		require.NoError(t, m.saveMetadata(&metadata{StoredMetadata: StoredMetadata{
			Id:        id,
			Name:      id,
			CreatedAt: now.Add(-time.Hour),
			ExpiresAt: &expiredAt,
			DataDir:   p.InstanceDir(id),
		}}))
	}

	calls := make(chan string, 2)
	releaseStuckDelete := make(chan struct{})
	stuckDeleteDone := make(chan struct{})
	m.deleteInstanceFn = func(_ context.Context, id string) error {
		calls <- id
		if id == "a-timeout" {
			<-releaseStuckDelete
			err := m.deleteInstanceData(id)
			close(stuckDeleteDone)
			return err
		}
		return m.deleteInstanceData(id)
	}

	m.reapExpiredInstances(context.Background())

	assert.Equal(t, "a-timeout", <-calls)
	assert.Equal(t, "b-deleted", <-calls)
	_, err := os.Stat(p.InstanceDir("a-timeout"))
	require.NoError(t, err)
	_, err = os.Stat(p.InstanceDir("b-deleted"))
	require.ErrorIs(t, err, os.ErrNotExist)

	close(releaseStuckDelete)
	select {
	case <-stuckDeleteDone:
	case <-time.After(time.Second):
		t.Fatal("timed-out deletion did not finish after release")
	}
}

func TestReapExpiredInstanceSkipsBusyCreateLock(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	p := paths.New(t.TempDir())
	m := &manager{paths: p, now: func() time.Time { return now }}
	id := "creating-instance"
	expiredAt := now.Add(-time.Minute)
	require.NoError(t, m.ensureDirectories(id))
	require.NoError(t, m.saveMetadata(&metadata{StoredMetadata: StoredMetadata{
		Id:        id,
		Name:      id,
		CreatedAt: now.Add(-time.Hour),
		ExpiresAt: &expiredAt,
		DataDir:   p.InstanceDir(id),
	}}))

	lock := m.getInstanceLock(id)
	lock.Lock()
	deleted, err := m.reapExpiredInstance(context.Background(), id)
	lock.Unlock()

	require.NoError(t, err)
	assert.False(t, deleted)
	_, err = os.Stat(p.InstanceDir(id))
	require.NoError(t, err)
}
