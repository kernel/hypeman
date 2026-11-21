package images

import (
	"context"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/kernel/hypeman/lib/paths"
	"github.com/kernel/hypeman/lib/queue"
	"github.com/stretchr/testify/require"
)

// fillQueue replaces m's queue with one whose single worker is blocked and
// whose pending slots are all taken, so the next Enqueue reports a full queue.
func fillQueue(t *testing.T, m *manager) {
	t.Helper()

	q := queue.New(1)
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })

	require.Equal(t, 0, q.Enqueue("blocker", func() { <-release }, nil))
	for i := 0; i < queue.MaxPending; i++ {
		require.Greater(t, q.Enqueue(fmt.Sprintf("filler-%d", i), func() {}, nil), 0)
	}
	m.queue = q
}

func TestCreateImageRefusedByFullQueueDoesNotBlock(t *testing.T) {
	reg := registry.New()
	server := httptest.NewServer(reg)
	defer server.Close()
	host := strings.TrimPrefix(server.URL, "http://")

	var refs []string
	for _, path := range []string{"/library/full-a:latest", "/library/full-b:latest"} {
		ref := host + path
		parsed, err := name.ParseReference(ref, name.Insecure)
		require.NoError(t, err)
		img, err := random.Image(256, 1)
		require.NoError(t, err)
		require.NoError(t, remote.Write(parsed, img))
		refs = append(refs, ref)
	}

	mgr, err := NewManager(paths.New(t.TempDir()), 1, nil)
	require.NoError(t, err)
	m := mgr.(*manager)
	fillQueue(t, m)

	ctx := context.Background()
	callCreate := func(ref string) error {
		t.Helper()
		done := make(chan error, 1)
		go func() {
			_, err := mgr.CreateImage(ctx, CreateImageRequest{Name: ref})
			done <- err
		}()
		select {
		case err := <-done:
			return err
		case <-time.After(10 * time.Second):
			t.Fatalf("CreateImage(%s) blocked while the queue was full", ref)
			return nil
		}
	}

	for _, ref := range refs {
		require.ErrorIs(t, callCreate(ref), queue.ErrFull, "create of %s must surface the refusal", ref)
	}

	// The manager must still be usable: another create is refused promptly
	// instead of deadlocking, and a delete completes.
	require.ErrorIs(t, callCreate(refs[0]), queue.ErrFull)

	done := make(chan error, 1)
	go func() { done <- mgr.DeleteImage(ctx, refs[0]) }()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("DeleteImage blocked after a refused create")
	}
}

func TestRefusedSuccessorKeepsPreviousInflightPull(t *testing.T) {
	mgr, err := NewManager(paths.New(t.TempDir()), 1, nil)
	require.NoError(t, err)
	m := mgr.(*manager)
	fillQueue(t, m)
	m.borrowedCredentialsTimeout = time.Second

	normalized, err := ParseNormalizedRef("registry.example/library/successor:latest")
	require.NoError(t, err)
	const digest = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	ref := NewResolvedRef(normalized, digest)
	credentials := &authn.AuthConfig{Username: "borrower", Password: "secret"}

	m.createMu.Lock()
	previous, _ := m.registerInflightPull(ref.Digest(), credentials)
	_, err = m.createAndQueueImage(ref, CreateImageRequest{Name: normalized.String(), Credentials: credentials}, hostPlatform())
	m.createMu.Unlock()
	require.ErrorIs(t, err, queue.ErrFull)

	// The pull that was already running keeps its handle, so it still builds.
	gotCredentials, _, expired, stale := m.borrowedAuth(ref.Digest(), previous)
	require.False(t, stale, "the running pull must stay current after a refused successor")
	require.False(t, expired)
	require.NotNil(t, gotCredentials)

	// Its expiry bound is still enforced on the restored handle.
	require.Eventually(t, func() bool {
		_, _, expired, stale := m.borrowedAuth(ref.Digest(), previous)
		return expired && !stale
	}, 5*time.Second, 10*time.Millisecond)

	// The refused successor's record is terminal instead of pending with no
	// runner.
	meta, err := readMetadata(m.paths, ref.Repository(), ref.DigestHex())
	require.NoError(t, err)
	require.Equal(t, StatusFailed, meta.Status)
	require.NotNil(t, meta.Error)
	require.Contains(t, *meta.Error, queue.ErrFull.Error())
}
