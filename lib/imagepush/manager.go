package imagepush

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/kernel/hypeman/lib/images"
	"github.com/kernel/hypeman/lib/paths"
	"github.com/kernel/hypeman/lib/registrypush"
	"github.com/nrednav/cuid2"
)

type inflightPush struct {
	id     string
	digest string
}

type manager struct {
	paths    *paths.Paths
	resolver ImageResolver
	provider registrypush.Provider
	queue    *pushQueue

	mu       sync.Mutex
	inflight map[string]inflightPush // key = pushKey(digest, target)

	subscriberMu sync.RWMutex
	subscribers  map[string][]chan StatusEvent // keyed by push ID
}

// NewManager creates a push manager. provider may be nil, in which case
// credentials resolve from the default Docker keychain. Interrupted pushes
// from a previous run are re-enqueued FIFO.
func NewManager(p *paths.Paths, resolver ImageResolver, provider registrypush.Provider, maxConcurrent int) (Manager, error) {
	if resolver == nil {
		return nil, fmt.Errorf("image resolver is required")
	}
	if provider == nil {
		provider = &registrypush.KeychainProvider{}
	}

	m := &manager{
		paths:       p,
		resolver:    resolver,
		provider:    provider,
		queue:       newPushQueue(maxConcurrent),
		inflight:    make(map[string]inflightPush),
		subscribers: make(map[string][]chan StatusEvent),
	}

	m.recoverInterruptedPushes()
	return m, nil
}

// pushKey identifies in-flight work by digest and target.
func pushKey(digest, target string) string {
	return digest + "->" + target
}

func (m *manager) CreatePush(ctx context.Context, req PushRequest) (*Push, error) {
	if req.Image == "" {
		return nil, fmt.Errorf("%w: image is required", images.ErrInvalidName)
	}

	img, err := m.resolver.GetImage(ctx, req.Image)
	if err != nil {
		return nil, err
	}
	if img.Status != images.StatusReady {
		return nil, fmt.Errorf("%w: %s is %s", ErrImageNotReady, img.Name, img.Status)
	}

	// Validate the target before persisting anything so typos fail fast.
	var refOpts []name.Option
	if req.Insecure {
		refOpts = append(refOpts, name.Insecure)
	}
	dstRef, err := name.ParseReference(req.Target, refOpts...)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidTarget, err)
	}

	key := pushKey(img.Digest, dstRef.String())

	// Hold the lock across dedup check, metadata write, and registration so a
	// concurrent request for the same digest+target cannot slip in between and
	// leave an orphaned queued job behind.
	m.mu.Lock()
	if existing, ok := m.inflight[key]; ok {
		id := existing.id
		m.mu.Unlock()
		return m.GetPush(ctx, id)
	}

	meta := &pushMetadata{
		ID:             cuid2.Generate(),
		Status:         StatusQueued,
		Image:          img.Name,
		Digest:         img.Digest,
		Target:         dstRef.String(),
		Insecure:       req.Insecure,
		HadCredentials: req.Credentials != nil,
		CreatedAt:      time.Now(),
	}
	if err := writeMetadata(m.paths, meta); err != nil {
		m.mu.Unlock()
		return nil, fmt.Errorf("write initial metadata: %w", err)
	}
	m.inflight[key] = inflightPush{id: meta.ID, digest: meta.Digest}
	m.mu.Unlock()

	// Borrowed credentials live only in this closure: the job provider is
	// built per push and never touches disk.
	provider := m.provider
	if req.Credentials != nil {
		provider = &registrypush.StaticProvider{Config: *req.Credentials}
	}

	metaCopy := *meta
	queuePos := m.queue.Enqueue(key, func() {
		m.executePush(context.Background(), &metaCopy, provider)
	})

	push := meta.toPush()
	if queuePos > 0 {
		push.QueuePosition = &queuePos
	}
	return push, nil
}

func (m *manager) executePush(ctx context.Context, meta *pushMetadata, provider registrypush.Provider) {
	key := pushKey(meta.Digest, meta.Target)
	defer func() {
		m.mu.Lock()
		delete(m.inflight, key)
		m.mu.Unlock()
	}()

	meta.Status = StatusPushing
	writeMetadata(m.paths, meta)

	result, err := registrypush.PushFromCache(ctx, m.paths, meta.Digest, meta.Target, provider, registrypush.Options{
		Insecure: meta.Insecure,
	})
	now := time.Now()
	if err != nil {
		errorMsg := err.Error()
		meta.Status = StatusFailed
		meta.Error = &errorMsg
		meta.CompletedAt = &now
		writeMetadata(m.paths, meta)
		m.notify(meta.ID, StatusFailed, err)
		return
	}

	meta.Status = StatusPushed
	meta.Layers = result.Layers
	meta.Bytes = result.Bytes
	meta.CompletedAt = &now
	writeMetadata(m.paths, meta)
	m.notify(meta.ID, StatusPushed, nil)
}

func (m *manager) GetPush(ctx context.Context, id string) (*Push, error) {
	meta, err := readMetadata(m.paths, id)
	if err != nil {
		return nil, err
	}

	push := meta.toPush()
	if meta.Status == StatusQueued {
		push.QueuePosition = m.queue.GetPosition(pushKey(meta.Digest, meta.Target))
	}
	return push, nil
}

func (m *manager) ListPushes(ctx context.Context) ([]Push, error) {
	metas, err := listAllPushes(m.paths)
	if err != nil {
		return nil, err
	}

	pushes := make([]Push, 0, len(metas))
	for _, meta := range metas {
		push := meta.toPush()
		if meta.Status == StatusQueued {
			push.QueuePosition = m.queue.GetPosition(pushKey(meta.Digest, meta.Target))
		}
		pushes = append(pushes, *push)
	}
	return pushes, nil
}

// WaitForPush blocks until the push reaches a terminal state (pushed or
// failed) or the context is cancelled.
func (m *manager) WaitForPush(ctx context.Context, id string) error {
	push, err := m.GetPush(ctx, id)
	if err != nil {
		return err
	}

	switch push.Status {
	case StatusPushed:
		return nil
	case StatusFailed:
		return pushError(push)
	}

	ch := make(chan StatusEvent, 1)
	m.subscribe(id, ch)
	defer m.unsubscribe(id, ch)

	// Re-check after subscribing to close the race window.
	push, err = m.GetPush(ctx, id)
	if err != nil {
		return err
	}
	switch push.Status {
	case StatusPushed:
		return nil
	case StatusFailed:
		return pushError(push)
	}

	select {
	case event := <-ch:
		if event.Status == StatusPushed {
			return nil
		}
		if event.Err != nil {
			return fmt.Errorf("push failed: %w", event.Err)
		}
		return fmt.Errorf("push failed")
	case <-ctx.Done():
		return ctx.Err()
	}
}

func pushError(push *Push) error {
	if push.Error != nil {
		return fmt.Errorf("push failed: %s", *push.Error)
	}
	return fmt.Errorf("push failed")
}

func (m *manager) InProgressDigests() []string {
	m.mu.Lock()
	defer m.mu.Unlock()

	seen := make(map[string]struct{}, len(m.inflight))
	digests := make([]string, 0, len(m.inflight))
	for _, job := range m.inflight {
		if _, ok := seen[job.digest]; ok {
			continue
		}
		seen[job.digest] = struct{}{}
		digests = append(digests, job.digest)
	}
	return digests
}

func (m *manager) recoverInterruptedPushes() {
	pending, err := listPendingPushes(m.paths)
	if err != nil {
		return // Best effort
	}

	for _, meta := range pending {
		// Borrowed credentials do not survive a restart, so a credentialed
		// push cannot be retried faithfully; fail it instead of re-enqueueing
		// with the default provider.
		if meta.HadCredentials {
			meta.Status = StatusFailed
			errorMsg := "push interrupted by restart: borrowed registry credentials are no longer available, retry the push"
			meta.Error = &errorMsg
			now := time.Now()
			meta.CompletedAt = &now
			writeMetadata(m.paths, meta)
			continue
		}

		key := pushKey(meta.Digest, meta.Target)

		m.mu.Lock()
		m.inflight[key] = inflightPush{id: meta.ID, digest: meta.Digest}
		m.mu.Unlock()

		metaCopy := *meta
		m.queue.Enqueue(key, func() {
			m.executePush(context.Background(), &metaCopy, m.provider)
		})
	}
}

func (m *manager) subscribe(id string, ch chan StatusEvent) {
	m.subscriberMu.Lock()
	defer m.subscriberMu.Unlock()
	m.subscribers[id] = append(m.subscribers[id], ch)
}

func (m *manager) unsubscribe(id string, ch chan StatusEvent) {
	m.subscriberMu.Lock()
	defer m.subscriberMu.Unlock()

	subs := m.subscribers[id]
	for i, sub := range subs {
		if sub == ch {
			m.subscribers[id] = append(subs[:i], subs[i+1:]...)
			break
		}
	}
	if len(m.subscribers[id]) == 0 {
		delete(m.subscribers, id)
	}
}

func (m *manager) notify(id, status string, err error) {
	m.subscriberMu.RLock()
	defer m.subscriberMu.RUnlock()

	event := StatusEvent{Status: status, Err: err}
	for _, ch := range m.subscribers[id] {
		select {
		case ch <- event:
		default:
		}
	}
}
