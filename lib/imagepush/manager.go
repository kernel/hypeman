package imagepush

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/kernel/hypeman/lib/images"
	"github.com/kernel/hypeman/lib/paths"
	"github.com/kernel/hypeman/lib/queue"
	"github.com/kernel/hypeman/lib/registrypush"
	"github.com/nrednav/cuid2"
)

type inflightPush struct {
	id             string
	digest         string
	hadCredentials bool
}

type manager struct {
	paths    *paths.Paths
	resolver ImageResolver
	provider registrypush.Provider
	queue    *queue.Queue

	mu       sync.Mutex
	inflight map[string]inflightPush // key = pushKey(digest, target, insecure)

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
		queue:       queue.New(maxConcurrent),
		inflight:    make(map[string]inflightPush),
		subscribers: make(map[string][]chan StatusEvent),
	}

	m.recoverInterruptedPushes()
	return m, nil
}

// pushKey identifies in-flight work by digest, target, and transport mode:
// the same target pushed with and without Insecure is distinct work.
func pushKey(digest, target string, insecure bool) string {
	if insecure {
		return digest + "->" + target + "+insecure"
	}
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
	// A ready image must carry a digest; without one the dedup key below
	// would collide across unrelated images.
	if img.Digest == "" {
		return nil, fmt.Errorf("%w: image %s has no digest", ErrImageNotReady, img.Name)
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

	key := pushKey(img.Digest, dstRef.String(), req.Insecure)

	// Borrowed credentials live only in this closure: the job provider is
	// built per push and never touches disk.
	provider := m.provider
	if req.Credentials != nil {
		provider = &registrypush.StaticProvider{Config: *req.Credentials}
	}

	// Hold the lock across the dedup check, metadata write, and registration so
	// a concurrent request for the same digest+target cannot slip in between
	// and create a duplicate job.
	m.mu.Lock()
	if existing, ok := m.inflight[key]; ok {
		// Merge only when the credential intent matches the in-flight job. The
		// manager never stores credential values, so it can only compare
		// presence: a request that borrowed credentials cannot merge into an
		// anonymous in-flight push (its auth would be silently dropped), and an
		// anonymous request cannot merge into a credentialed one (it would
		// silently inherit another caller's login). Both silently merge under
		// the wrong auth otherwise; surface the conflict instead so the caller
		// can retry once the in-flight job completes or match its credentials.
		hadCreds := req.Credentials != nil
		if existing.hadCredentials != hadCreds {
			m.mu.Unlock()
			return nil, fmt.Errorf("%w: a push of %s to %s is already in flight with different credentials; retry once it completes or match its credentials", ErrCredentialConflict, img.Digest, dstRef.String())
		}
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
	m.inflight[key] = inflightPush{id: meta.ID, digest: meta.Digest, hadCredentials: meta.HadCredentials}
	m.mu.Unlock()

	metaCopy := *meta
	queuePos := m.queue.Enqueue(key, func() {
		m.executePush(context.Background(), &metaCopy, provider)
	}, m.releaseInflight(key))

	push := meta.toPush()
	if queuePos > 0 {
		push.QueuePosition = &queuePos
	}
	return push, nil
}

func (m *manager) executePush(ctx context.Context, meta *pushMetadata, provider registrypush.Provider) {
	// Contain panics in the job goroutine: record a failed terminal and
	// notify waiters instead of leaving the job stuck as pushing. The queue
	// slot is released by its own deferred completion.
	defer func() {
		if r := recover(); r != nil {
			fmt.Fprintf(os.Stderr, "Warning: push %s to %s panicked: %v\n", meta.ID, meta.Target, r)
			now := time.Now()
			errorMsg := fmt.Sprintf("push panicked: %v", r)
			meta.Status = StatusFailed
			meta.Error = &errorMsg
			meta.CompletedAt = &now
			if err := m.writeTerminal(meta); err != nil {
				os.RemoveAll(m.paths.PushDir(meta.ID))
			}
			m.notify(meta.ID, StatusFailed, fmt.Errorf("push panicked: %v", r))
		}
	}()

	meta.Status = StatusPushing
	// Best-effort: if this write fails the job still runs and the terminal
	// record written on completion is the source of truth for recovery.
	if err := writeMetadata(m.paths, meta); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to persist push status for %s: %v\n", meta.ID, err)
	}

	result, pushErr := registrypush.PushFromCache(ctx, m.paths, meta.Digest, meta.Target, provider, registrypush.Options{
		Insecure: meta.Insecure,
	})
	now := time.Now()
	if pushErr != nil {
		errorMsg := pushErr.Error()
		meta.Status = StatusFailed
		meta.Error = &errorMsg
	} else {
		meta.Status = StatusPushed
		meta.Layers = result.Layers
		meta.Bytes = result.Bytes
	}
	meta.CompletedAt = &now

	if err := m.writeTerminal(meta); err != nil {
		// The outcome cannot be recorded: drop the record and report the job
		// as failed with the persistence problem, so WaitForPush and GetPush
		// agree instead of diverging into success-then-not-found. The actual
		// push outcome goes to the log.
		fmt.Fprintf(os.Stderr, "Warning: push %s to %s finished as %s but the job record could not be persisted: %v\n", meta.ID, meta.Target, strings.ToLower(meta.Status), err)
		os.RemoveAll(m.paths.PushDir(meta.ID))
		persistErr := fmt.Errorf("job record could not be persisted: %w", err)
		errorMsg := persistErr.Error()
		meta.Status = StatusFailed
		meta.Error = &errorMsg
		m.notify(meta.ID, StatusFailed, persistErr)
		return
	}

	if pushErr != nil {
		m.notify(meta.ID, StatusFailed, pushErr)
	} else {
		m.notify(meta.ID, StatusPushed, nil)
	}
}

// writeTerminal persists a terminal status, retrying once.
func (m *manager) writeTerminal(meta *pushMetadata) error {
	err := writeMetadata(m.paths, meta)
	if err != nil {
		err = writeMetadata(m.paths, meta)
	}
	return err
}

// releaseInflight returns the queue completion hook that drops the job's
// inflight registration. The queue runs it only after the key leaves the
// active set, so a concurrent CreatePush never falls into a gap between job
// completion and slot release: it either sees the inflight entry and gets the
// finished job, or enqueues a fresh one that actually starts.
// This relies on executePush having persisted the terminal status before it
// returns (including from its panic handler); otherwise the record on disk
// and the inflight map could disagree about whether the job is still running.
func (m *manager) releaseInflight(key string) func() {
	return func() {
		m.mu.Lock()
		delete(m.inflight, key)
		m.mu.Unlock()
	}
}

func (m *manager) GetPush(ctx context.Context, id string) (*Push, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	meta, err := readMetadata(m.paths, id)
	if err != nil {
		return nil, err
	}

	push := meta.toPush()
	if meta.Status == StatusQueued {
		push.QueuePosition = m.queue.GetPosition(pushKey(meta.Digest, meta.Target, meta.Insecure))
	}
	return push, nil
}

func (m *manager) ListPushes(ctx context.Context) ([]Push, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	metas, err := listAllPushes(m.paths)
	if err != nil {
		return nil, err
	}

	pushes := make([]Push, 0, len(metas))
	for _, meta := range metas {
		push := meta.toPush()
		if meta.Status == StatusQueued {
			push.QueuePosition = m.queue.GetPosition(pushKey(meta.Digest, meta.Target, meta.Insecure))
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
		// Loud on purpose: without recovery these records stay queued on disk
		// with nothing to run them.
		fmt.Fprintf(os.Stderr, "Warning: could not recover interrupted pushes: %v\n", err)
		return
	}

	seen := make(map[string]string, len(pending)) // key -> recovered push ID
	for _, meta := range pending {
		key := pushKey(meta.Digest, meta.Target, meta.Insecure)

		// Borrowed credentials do not survive a restart, so a credentialed
		// push cannot be retried faithfully; fail it instead of re-enqueueing
		// with the default provider.
		if meta.HadCredentials {
			m.failRecovered(meta, "push interrupted by restart: borrowed registry credentials are no longer available, retry the push")
			continue
		}

		// Duplicate records for one logical push can accumulate on disk;
		// recover the oldest and close the rest so nothing is left forever
		// queued.
		if chosen, ok := seen[key]; ok {
			m.failRecovered(meta, fmt.Sprintf("superseded by duplicate push job %s for the same image and target", chosen))
			continue
		}
		seen[key] = meta.ID

		m.mu.Lock()
		m.inflight[key] = inflightPush{id: meta.ID, digest: meta.Digest, hadCredentials: meta.HadCredentials}
		m.mu.Unlock()

		metaCopy := *meta
		m.queue.Enqueue(key, func() {
			m.executePush(context.Background(), &metaCopy, m.provider)
		}, m.releaseInflight(key))
	}
}

// failRecovered marks a recovered job failed. If the status cannot be
// persisted, the record is removed instead of being left permanently queued.
// Subscribers are notified so a WaitForPush racing the close does not hang.
func (m *manager) failRecovered(meta *pushMetadata, reason string) {
	meta.Status = StatusFailed
	meta.Error = &reason
	now := time.Now()
	meta.CompletedAt = &now
	if err := m.writeTerminal(meta); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: dropping unrecoverable push record %s: %v\n", meta.ID, err)
		os.RemoveAll(m.paths.PushDir(meta.ID))
	}
	m.notify(meta.ID, StatusFailed, errors.New(reason))
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
