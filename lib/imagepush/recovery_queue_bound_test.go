package imagepush

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/kernel/hypeman/lib/images"
	"github.com/kernel/hypeman/lib/queue"
	"github.com/kernel/hypeman/lib/registrypush"
	"github.com/stretchr/testify/require"
)

// fullQueue returns a queue whose single worker is blocked and whose pending
// slots are all taken, so the next Enqueue is refused. drain releases the
// blocker so the queue empties.
func fullQueue(t *testing.T) (q *queue.Queue, drain func()) {
	t.Helper()

	q = queue.New(1)
	release := make(chan struct{})
	var once sync.Once
	drain = func() {
		once.Do(func() { close(release) })
		require.Eventually(t, func() bool { return q.QueueLength() == 0 }, 10*time.Second, 10*time.Millisecond)
	}
	t.Cleanup(drain)

	require.Equal(t, 0, q.Enqueue("blocker", func() { <-release }, nil))
	for i := 0; i < queue.MaxPending; i++ {
		require.Greater(t, q.Enqueue(fmt.Sprintf("filler-%d", i), func() {}, nil), 0)
	}
	return q, drain
}

func TestRecoveryRefusedByFullQueueLeavesNoInflightEntry(t *testing.T) {
	p, digest := cacheFixture(t)
	host := openRegistry(t)
	target := host + "/export/recovered:v1"

	meta := &pushMetadata{
		ID:        "recovered-push",
		Status:    StatusPushing,
		Image:     "myapp:v1",
		Digest:    digest,
		Target:    target,
		Insecure:  true,
		CreatedAt: time.Now(),
	}
	writePushes(t, p, meta)

	q, drain := fullQueue(t)
	mgr := &manager{
		paths: p,
		resolver: &fakeResolver{images: map[string]*images.Image{
			"myapp:v1": readyImage("myapp:v1", digest),
		}},
		provider:    &registrypush.KeychainProvider{},
		queue:       q,
		pushTimeout: DefaultPushTimeout,
		inflight:    make(map[string]inflightPush),
		subscribers: make(map[string][]chan StatusEvent),
	}
	mgr.recoverInterruptedPushes()

	// The refused record is terminal so it cannot sit queued forever.
	failed, err := readMetadata(p, "recovered-push")
	require.NoError(t, err)
	require.Equal(t, StatusFailed, failed.Status)
	require.NotNil(t, failed.Error)
	require.Contains(t, *failed.Error, "queue is full")

	// No reservation and no queued job survived the refusal.
	key := pushKey(digest, target, true)
	mgr.mu.Lock()
	_, inflight := mgr.inflight[key]
	mgr.mu.Unlock()
	require.False(t, inflight, "the refused recovery must drop its inflight entry")
	require.Equal(t, 1+queue.MaxPending, q.QueueLength(), "the refused job must not be queued")

	// Once the queue drains, a CreatePush for the same digest and target runs
	// as a fresh job instead of handing back the failed record.
	drain()
	push, err := mgr.CreatePush(context.Background(), PushRequest{
		Image: "myapp:v1", Target: target, Insecure: true,
	})
	require.NoError(t, err)
	require.NotEmpty(t, push.ID)
	require.NotEqual(t, "recovered-push", push.ID, "a retry must not dedup onto the failed record")

	mustPushed(t, mgr, push.ID)
	waitNoInflight(t, mgr)
}
