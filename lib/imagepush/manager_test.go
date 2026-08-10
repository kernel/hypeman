package imagepush

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/types"
	"github.com/kernel/hypeman/lib/images"
	"github.com/kernel/hypeman/lib/ocicache"
	"github.com/kernel/hypeman/lib/paths"
	"github.com/kernel/hypeman/lib/registrypush"
)

// fakeResolver resolves image names from a fixed map.
type fakeResolver struct {
	images map[string]*images.Image
	err    error
}

func (f *fakeResolver) GetImage(_ context.Context, name string) (*images.Image, error) {
	if f.err != nil {
		return nil, f.err
	}
	img, ok := f.images[name]
	if !ok {
		return nil, fmt.Errorf("%w: %s", images.ErrNotFound, name)
	}
	return img, nil
}

// cacheFixture writes an OCI-native random image into a temp OCI cache and
// returns the paths and manifest digest.
func cacheFixture(t *testing.T) (*paths.Paths, string) {
	t.Helper()

	p := paths.New(t.TempDir())
	randomImg, err := random.Image(256, 1)
	if err != nil {
		t.Fatalf("random image: %v", err)
	}
	img := mutate.MediaType(randomImg, types.OCIManifestSchema1)

	blobDir := p.OCICacheBlobDir()
	if err := os.MkdirAll(blobDir, 0755); err != nil {
		t.Fatalf("create blob dir: %v", err)
	}
	writeBlob := func(hash v1.Hash, data []byte) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(blobDir, hash.Hex), data, 0644); err != nil {
			t.Fatalf("write blob: %v", err)
		}
	}

	layers, err := img.Layers()
	if err != nil {
		t.Fatalf("layers: %v", err)
	}
	for _, layer := range layers {
		rc, err := layer.Compressed()
		if err != nil {
			t.Fatalf("layer reader: %v", err)
		}
		data, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			t.Fatalf("read layer: %v", err)
		}
		hash, err := layer.Digest()
		if err != nil {
			t.Fatalf("layer digest: %v", err)
		}
		writeBlob(hash, data)
	}

	rawConfig, err := img.RawConfigFile()
	if err != nil {
		t.Fatalf("raw config: %v", err)
	}
	configHash, err := img.ConfigName()
	if err != nil {
		t.Fatalf("config name: %v", err)
	}
	writeBlob(configHash, rawConfig)

	rawManifest, err := img.RawManifest()
	if err != nil {
		t.Fatalf("raw manifest: %v", err)
	}
	digest, err := img.Digest()
	if err != nil {
		t.Fatalf("digest: %v", err)
	}
	writeBlob(digest, rawManifest)

	return p, digest.String()
}

// gatedRegistry serves an in-process registry whose requests block until the
// returned channel is closed.
func gatedRegistry(t *testing.T) (host string, gate chan struct{}) {
	t.Helper()
	gate = make(chan struct{})
	inner := registry.New()
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-gate
		inner.ServeHTTP(w, r)
	})
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return strings.TrimPrefix(srv.URL, "http://"), gate
}

// openRegistry serves an unblocked in-process registry.
func openRegistry(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(registry.New())
	t.Cleanup(srv.Close)
	return strings.TrimPrefix(srv.URL, "http://")
}

func readyImage(name_, digest string) *images.Image {
	return &images.Image{
		Name:   name_,
		Digest: digest,
		Status: images.StatusReady,
	}
}

// testManager wires the standard fixture: a temp OCI cache containing a ready
// random image and a resolver mapping "myapp:v1" to it. provider and resolver
// may be nil (default keychain provider / ready image map). Returns the
// manager and the image's manifest digest.
func testManager(t *testing.T, maxConcurrent int, provider registrypush.Provider, resolver ImageResolver) (Manager, string) {
	t.Helper()
	p, digest := cacheFixture(t)
	if resolver == nil {
		resolver = &fakeResolver{images: map[string]*images.Image{
			"myapp:v1": readyImage("myapp:v1", digest),
		}}
	}
	mgr, err := NewManager(p, resolver, provider, maxConcurrent)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	return mgr, digest
}

// waitTerminal polls GetPush until the push reaches a terminal (pushed or
// failed) state and returns it. It replaces the removed WaitForPush surface:
// pushes complete asynchronously and tests observe the result by polling.
func waitTerminal(t *testing.T, mgr Manager, id string) *Push {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		got, err := mgr.GetPush(context.Background(), id)
		if err != nil {
			t.Fatalf("GetPush %s: %v", id, err)
		}
		if got.Status == StatusPushed || got.Status == StatusFailed {
			return got
		}
		if time.Now().After(deadline) {
			t.Fatalf("push %s never reached a terminal state (status=%s)", id, got.Status)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// mustPushed waits for the push to reach a terminal pushed state and returns
// it, failing the test otherwise.
func mustPushed(t *testing.T, mgr Manager, id string) *Push {
	t.Helper()
	got := waitTerminal(t, mgr, id)
	if got.Status != StatusPushed {
		t.Fatalf("push %s status = %s, want pushed (error: %v)", id, got.Status, got.Error)
	}
	return got
}

func writePushes(t *testing.T, p *paths.Paths, metas ...*pushMetadata) {
	t.Helper()
	for _, meta := range metas {
		if err := writeMetadata(p, meta); err != nil {
			t.Fatalf("writeMetadata(%s): %v", meta.ID, err)
		}
	}
}

func TestCreatePushEndToEnd(t *testing.T) {
	mgr, digest := testManager(t, 2, nil, nil)
	host := openRegistry(t)

	target := host + "/export/app:v1"
	push, err := mgr.CreatePush(context.Background(), PushRequest{Image: "myapp:v1", Target: target, Insecure: true})
	if err != nil {
		t.Fatalf("CreatePush: %v", err)
	}
	if push.Status != StatusQueued && push.Status != StatusPushing {
		t.Errorf("initial status = %s, want queued or pushing", push.Status)
	}

	got := mustPushed(t, mgr, push.ID)
	if got.Digest != digest {
		t.Errorf("digest = %s, want %s", got.Digest, digest)
	}
	if got.Bytes <= 0 || got.Layers != 1 {
		t.Errorf("bytes/layers = %d/%d, want >0/1", got.Bytes, got.Layers)
	}
	if got.CompletedAt == nil {
		t.Error("completed at not set")
	}

	// The pushed image must be readable from the destination with the same digest.
	dstRef, err := name.ParseReference(target, name.Insecure)
	if err != nil {
		t.Fatalf("parse target: %v", err)
	}
	desc, err := remote.Get(dstRef, remote.WithAuth(authn.Anonymous))
	if err != nil {
		t.Fatalf("read back pushed image: %v", err)
	}
	if desc.Digest.String() != digest {
		t.Errorf("pushed digest = %s, want %s", desc.Digest, digest)
	}

	// No in-flight digests once done.
	if digests := mgr.(*manager).inProgressDigests(); len(digests) != 0 {
		t.Errorf("inProgressDigests = %v, want empty", digests)
	}
}

func TestCreatePushDedupesInFlight(t *testing.T) {
	mgr, digest := testManager(t, 2, nil, nil)
	host, gate := gatedRegistry(t)

	req := PushRequest{Image: "myapp:v1", Target: host + "/export/app:v1", Insecure: true}
	first, err := mgr.CreatePush(context.Background(), req)
	if err != nil {
		t.Fatalf("first CreatePush: %v", err)
	}

	// Same digest+target while in flight returns the same job.
	second, err := mgr.CreatePush(context.Background(), req)
	if err != nil {
		t.Fatalf("second CreatePush: %v", err)
	}
	if second.ID != first.ID {
		t.Errorf("duplicate push got new ID %s, want %s", second.ID, first.ID)
	}

	if digests := mgr.(*manager).inProgressDigests(); len(digests) != 1 || digests[0] != digest {
		t.Errorf("inProgressDigests = %v, want [%s]", digests, digest)
	}

	close(gate)
	mustPushed(t, mgr, first.ID)
}

func TestCreatePushQueuesBehindConcurrencyLimit(t *testing.T) {
	mgr, _ := testManager(t, 1, nil, nil)
	host, gate := gatedRegistry(t)

	first, err := mgr.CreatePush(context.Background(), PushRequest{
		Image: "myapp:v1", Target: host + "/export/a:v1", Insecure: true,
	})
	if err != nil {
		t.Fatalf("first CreatePush: %v", err)
	}

	// Wait until the first job is actually running so the second must queue.
	deadline := time.Now().Add(5 * time.Second)
	for {
		got, err := mgr.GetPush(context.Background(), first.ID)
		if err != nil {
			t.Fatalf("GetPush: %v", err)
		}
		if got.Status == StatusPushing {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("first push never started")
		}
		time.Sleep(10 * time.Millisecond)
	}

	second, err := mgr.CreatePush(context.Background(), PushRequest{
		Image: "myapp:v1", Target: host + "/export/b:v1", Insecure: true,
	})
	if err != nil {
		t.Fatalf("second CreatePush: %v", err)
	}
	if second.QueuePosition == nil || *second.QueuePosition != 1 {
		t.Errorf("second queue position = %v, want 1", second.QueuePosition)
	}
	// The manager read surface reports the same pending position.
	got, err := mgr.GetPush(context.Background(), second.ID)
	if err != nil {
		t.Fatalf("GetPush pending: %v", err)
	}
	if got.QueuePosition == nil || *got.QueuePosition != 1 {
		t.Errorf("GetPush pending queue position = %v, want 1", got.QueuePosition)
	}

	close(gate)
	mustPushed(t, mgr, first.ID)
	mustPushed(t, mgr, second.ID)
}

func TestCreatePushRejectsInvalidRequests(t *testing.T) {
	cases := []struct {
		name     string
		resolver ImageResolver
		req      PushRequest
		want     error
	}{
		{
			name:     "image not ready",
			resolver: &fakeResolver{images: map[string]*images.Image{"myapp:v1": {Name: "myapp:v1", Digest: "sha256:0", Status: images.StatusConverting}}},
			req:      PushRequest{Image: "myapp:v1", Target: "registry.example.com/app:v1"},
			want:     ErrImageNotReady,
		},
		{
			name:     "unknown image",
			resolver: &fakeResolver{images: map[string]*images.Image{}},
			req:      PushRequest{Image: "missing:v1", Target: "registry.example.com/app:v1"},
			want:     images.ErrNotFound,
		},
		{
			name: "invalid target",
			req:  PushRequest{Image: "myapp:v1", Target: "!!!invalid"},
			want: ErrInvalidTarget,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mgr, _ := testManager(t, 1, nil, tc.resolver)
			if _, err := mgr.CreatePush(context.Background(), tc.req); !errors.Is(err, tc.want) {
				t.Errorf("err = %v, want %v", err, tc.want)
			}
		})
	}

	// Nothing was persisted for an invalid request.
	mgr, _ := testManager(t, 1, nil, nil)
	if _, err := mgr.CreatePush(context.Background(), PushRequest{Image: "myapp:v1", Target: "!!!invalid"}); !errors.Is(err, ErrInvalidTarget) {
		t.Fatalf("err = %v, want ErrInvalidTarget", err)
	}
	pushes, err := mgr.ListPushes(context.Background())
	if err != nil {
		t.Fatalf("ListPushes: %v", err)
	}
	if len(pushes) != 0 {
		t.Errorf("len(pushes) = %d, want 0", len(pushes))
	}
}

func TestCreatePushFailureRecorded(t *testing.T) {
	mgr, _ := testManager(t, 1, nil, nil)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	host := strings.TrimPrefix(srv.URL, "http://")

	push, err := mgr.CreatePush(context.Background(), PushRequest{
		Image: "myapp:v1", Target: host + "/export/app:v1", Insecure: true,
	})
	if err != nil {
		t.Fatalf("CreatePush: %v", err)
	}

	got := waitTerminal(t, mgr, push.ID)
	if got.Status != StatusFailed {
		t.Errorf("status = %s, want failed", got.Status)
	}
	if got.Error == nil || *got.Error == "" {
		t.Error("error not recorded on failed push")
	}
}

func TestListPushesNewestFirst(t *testing.T) {
	mgr, _ := testManager(t, 2, nil, nil)
	host := openRegistry(t)

	first, err := mgr.CreatePush(context.Background(), PushRequest{
		Image: "myapp:v1", Target: host + "/export/a:v1", Insecure: true,
	})
	if err != nil {
		t.Fatalf("CreatePush a: %v", err)
	}
	mustPushed(t, mgr, first.ID)

	second, err := mgr.CreatePush(context.Background(), PushRequest{
		Image: "myapp:v1", Target: host + "/export/b:v1", Insecure: true,
	})
	if err != nil {
		t.Fatalf("CreatePush b: %v", err)
	}
	mustPushed(t, mgr, second.ID)

	pushes, err := mgr.ListPushes(context.Background())
	if err != nil {
		t.Fatalf("ListPushes: %v", err)
	}
	if len(pushes) != 2 {
		t.Fatalf("len(pushes) = %d, want 2", len(pushes))
	}
	if pushes[0].ID != second.ID || pushes[1].ID != first.ID {
		t.Errorf("ordering = %s,%s, want newest first", pushes[0].ID, pushes[1].ID)
	}
}

func TestRecoverInterruptedPushes(t *testing.T) {
	p, digest := cacheFixture(t)
	host := openRegistry(t)

	// Simulate a push interrupted by a restart: metadata on disk, nothing queued.
	meta := &pushMetadata{
		ID:        "recovered-push",
		Status:    StatusPushing,
		Image:     "myapp:v1",
		Digest:    digest,
		Target:    host + "/export/recovered:v1",
		Insecure:  true,
		CreatedAt: time.Now(),
	}
	if err := writeMetadata(p, meta); err != nil {
		t.Fatalf("writeMetadata: %v", err)
	}

	resolver := &fakeResolver{images: map[string]*images.Image{
		"myapp:v1": readyImage("myapp:v1", digest),
	}}
	mgr, err := NewManager(p, resolver, nil, 1)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	mustPushed(t, mgr, "recovered-push")
}

func TestCreatePushDedupesConcurrently(t *testing.T) {
	mgr, digest := testManager(t, 2, nil, nil)
	host, gate := gatedRegistry(t)

	req := PushRequest{Image: "myapp:v1", Target: host + "/export/app:v1", Insecure: true}

	// Two goroutines racing CreatePush for the same digest+target — including
	// the registration itself — must both land on the single job: one
	// registers, the other merges. The gated registry keeps the winner in
	// flight so the race cannot resolve before both calls return.
	const n = 2
	ids := make([]string, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			push, err := mgr.CreatePush(context.Background(), req)
			if push != nil {
				ids[i] = push.ID
			}
			errs[i] = err
		}(i)
	}
	wg.Wait()

	for i := 0; i < n; i++ {
		if errs[i] != nil {
			t.Fatalf("concurrent CreatePush #%d: %v", i, errs[i])
		}
		if ids[i] != ids[0] {
			t.Errorf("concurrent push #%d got ID %s, want %s (single job)", i, ids[i], ids[0])
		}
	}

	if digests := mgr.(*manager).inProgressDigests(); len(digests) != 1 || digests[0] != digest {
		t.Errorf("inProgressDigests = %v, want [%s]", digests, digest)
	}

	close(gate)
	mustPushed(t, mgr, ids[0])
}

func TestCreatePushCredentialConflict(t *testing.T) {
	mgr, _ := testManager(t, 1, nil, nil)
	hostA, gateA := gatedRegistry(t)
	hostB, gateB := gatedRegistry(t)
	targetA := hostA + "/export/a:v1"
	targetB := hostB + "/export/b:v1"

	// Credentialed in-flight push, then an anonymous request: merging would
	// silently drop the request's intent and run under the in-flight auth.
	seeded, err := mgr.CreatePush(context.Background(), PushRequest{
		Image: "myapp:v1", Target: targetA, Insecure: true,
		Credentials: &authn.AuthConfig{RegistryToken: "borrowed-token"},
	})
	if err != nil {
		t.Fatalf("seed CreatePush: %v", err)
	}
	if _, err := mgr.CreatePush(context.Background(), PushRequest{Image: "myapp:v1", Target: targetA, Insecure: true}); !errors.Is(err, ErrCredentialConflict) {
		t.Errorf("anonymous duplicate err = %v, want ErrCredentialConflict", err)
	}

	// Reverse: anonymous in-flight push, then a credentialed request.
	seeded2, err := mgr.CreatePush(context.Background(), PushRequest{Image: "myapp:v1", Target: targetB, Insecure: true})
	if err != nil {
		t.Fatalf("seed CreatePush 2: %v", err)
	}
	if _, err := mgr.CreatePush(context.Background(), PushRequest{
		Image: "myapp:v1", Target: targetB, Insecure: true,
		Credentials: &authn.AuthConfig{RegistryToken: "borrowed-token"},
	}); !errors.Is(err, ErrCredentialConflict) {
		t.Errorf("credentialed duplicate err = %v, want ErrCredentialConflict", err)
	}

	close(gateA)
	close(gateB)
	mustPushed(t, mgr, seeded.ID)
	mustPushed(t, mgr, seeded2.ID)

	// The conflicted requests must not have created duplicate jobs: only the
	// two seeds exist.
	pushes, err := mgr.ListPushes(context.Background())
	if err != nil {
		t.Fatalf("ListPushes: %v", err)
	}
	if len(pushes) != 2 {
		t.Errorf("len(pushes) = %d, want 2 (conflicts created no jobs)", len(pushes))
	}
}

func TestCreatePushCredentialMismatch(t *testing.T) {
	mgr, _ := testManager(t, 1, nil, nil)
	host, gate := gatedRegistry(t)
	target := host + "/export/app:v1"

	seeded, err := mgr.CreatePush(context.Background(), PushRequest{
		Image: "myapp:v1", Target: target, Insecure: true,
		Credentials: &authn.AuthConfig{RegistryToken: "token-a"},
	})
	if err != nil {
		t.Fatalf("seed CreatePush: %v", err)
	}

	// A different borrowed login for the same in-flight work must conflict:
	// merging would run the request under token-a's principal.
	if _, err := mgr.CreatePush(context.Background(), PushRequest{
		Image: "myapp:v1", Target: target, Insecure: true,
		Credentials: &authn.AuthConfig{RegistryToken: "token-b"},
	}); !errors.Is(err, ErrCredentialConflict) {
		t.Errorf("mismatched-credential duplicate err = %v, want ErrCredentialConflict", err)
	}

	// The same borrowed login merges into the in-flight job.
	dup, err := mgr.CreatePush(context.Background(), PushRequest{
		Image: "myapp:v1", Target: target, Insecure: true,
		Credentials: &authn.AuthConfig{RegistryToken: "token-a"},
	})
	if err != nil {
		t.Fatalf("same-credential duplicate: %v", err)
	}
	if dup.ID != seeded.ID {
		t.Errorf("same-credential duplicate got ID %s, want %s", dup.ID, seeded.ID)
	}

	close(gate)
	mustPushed(t, mgr, seeded.ID)
}

func TestInProgressDigestsDedupesAcrossTargets(t *testing.T) {
	mgr, digest := testManager(t, 2, nil, nil)
	hostA, gateA := gatedRegistry(t)
	hostB, gateB := gatedRegistry(t)

	// The same image pushed to two targets is two jobs but one live digest:
	// the GC needs the digest kept alive once, not per target.
	pushes := make([]string, 0, 2)
	for _, target := range []string{hostA + "/export/a:v1", hostB + "/export/b:v1"} {
		push, err := mgr.CreatePush(context.Background(), PushRequest{Image: "myapp:v1", Target: target, Insecure: true})
		if err != nil {
			t.Fatalf("CreatePush %s: %v", target, err)
		}
		pushes = append(pushes, push.ID)
	}

	if digests := mgr.(*manager).inProgressDigests(); len(digests) != 1 || digests[0] != digest {
		t.Errorf("inProgressDigests = %v, want [%s]", digests, digest)
	}

	// Drain the gated jobs so their writes land before the fixture's TempDir
	// cleanup.
	close(gateA)
	close(gateB)
	for _, id := range pushes {
		mustPushed(t, mgr, id)
	}
}

func TestRecoveryDedupesSameKey(t *testing.T) {
	p, digest := cacheFixture(t)
	host := openRegistry(t)
	target := host + "/export/dup:v1"
	now := time.Now()

	older := &pushMetadata{ID: "older", Status: StatusQueued, Image: "myapp:v1", Digest: digest, Target: target, Insecure: true, CreatedAt: now.Add(-time.Minute)}
	newer := &pushMetadata{ID: "newer", Status: StatusQueued, Image: "myapp:v1", Digest: digest, Target: target, Insecure: true, CreatedAt: now}
	writePushes(t, p, older, newer)

	resolver := &fakeResolver{images: map[string]*images.Image{
		"myapp:v1": readyImage("myapp:v1", digest),
	}}
	mgr, err := NewManager(p, resolver, nil, 1)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	mustPushed(t, mgr, "older")

	got, err := mgr.GetPush(context.Background(), "newer")
	if err != nil {
		t.Fatalf("GetPush newer: %v", err)
	}
	if got.Status != StatusFailed {
		t.Errorf("newer status = %s, want failed (superseded)", got.Status)
	}
	if got.Error == nil || !strings.Contains(*got.Error, "duplicate of push job older") {
		t.Errorf("newer error = %v, want duplicate-of-older explanation", got.Error)
	}
	// The superseded job surfaces the failure rather than hanging.
	if got := waitTerminal(t, mgr, "newer"); got.Status != StatusFailed {
		t.Errorf("superseded job status = %s, want failed", got.Status)
	}
}

func TestSequentialSameKeyPushesAllComplete(t *testing.T) {
	mgr, _ := testManager(t, 1, nil, nil)
	host := openRegistry(t)

	target := host + "/export/again:v1"
	for i := 0; i < 5; i++ {
		push, err := mgr.CreatePush(context.Background(), PushRequest{
			Image: "myapp:v1", Target: target, Insecure: true,
		})
		if err != nil {
			t.Fatalf("CreatePush #%d: %v", i, err)
		}
		mustPushed(t, mgr, push.ID)
	}
}

// erroringProvider always fails, proving a push that succeeds used the
// request's borrowed credentials instead of the manager default.
type erroringProvider struct{}

func (erroringProvider) Authenticator(_ context.Context, _ name.Reference) (authn.Authenticator, error) {
	return nil, fmt.Errorf("default provider must not be used")
}

func TestCreatePushWithBorrowedCredentials(t *testing.T) {
	mgr, _ := testManager(t, 1, erroringProvider{}, nil)

	inner := registry.New()
	gated := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer borrowed-token" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		inner.ServeHTTP(w, r)
	})
	srv := httptest.NewServer(gated)
	t.Cleanup(srv.Close)
	host := strings.TrimPrefix(srv.URL, "http://")

	push, err := mgr.CreatePush(context.Background(), PushRequest{
		Image:       "myapp:v1",
		Target:      host + "/export/app:v1",
		Insecure:    true,
		Credentials: &authn.AuthConfig{RegistryToken: "borrowed-token"},
	})
	if err != nil {
		t.Fatalf("CreatePush: %v", err)
	}
	mustPushed(t, mgr, push.ID)
}

func TestCredentialsNeverPersisted(t *testing.T) {
	p, digest := cacheFixture(t)
	host := openRegistry(t)
	resolver := &fakeResolver{images: map[string]*images.Image{
		"myapp:v1": readyImage("myapp:v1", digest),
	}}
	mgr, err := NewManager(p, resolver, nil, 1)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	const secret = "super-secret-borrowed-password"
	push, err := mgr.CreatePush(context.Background(), PushRequest{
		Image:       "myapp:v1",
		Target:      host + "/export/app:v1",
		Insecure:    true,
		Credentials: &authn.AuthConfig{Username: "pusher", Password: secret},
	})
	if err != nil {
		t.Fatalf("CreatePush: %v", err)
	}
	mustPushed(t, mgr, push.ID)

	data, err := os.ReadFile(p.PushMetadata(push.ID))
	if err != nil {
		t.Fatalf("read metadata: %v", err)
	}
	if strings.Contains(string(data), secret) || strings.Contains(string(data), "pusher") {
		t.Error("borrowed credentials were persisted to disk")
	}
}

func TestRecoveryFailsCredentialJobs(t *testing.T) {
	p, digest := cacheFixture(t)
	host := openRegistry(t)

	meta := &pushMetadata{
		ID:             "cred-push",
		Status:         StatusQueued,
		Image:          "myapp:v1",
		Digest:         digest,
		Target:         host + "/export/recovered:v1",
		Insecure:       true,
		HadCredentials: true,
		CreatedAt:      time.Now(),
	}
	if err := writeMetadata(p, meta); err != nil {
		t.Fatalf("writeMetadata: %v", err)
	}

	resolver := &fakeResolver{images: map[string]*images.Image{
		"myapp:v1": readyImage("myapp:v1", digest),
	}}
	mgr, err := NewManager(p, resolver, nil, 1)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	got, err := mgr.GetPush(context.Background(), "cred-push")
	if err != nil {
		t.Fatalf("GetPush: %v", err)
	}
	if got.Status != StatusFailed {
		t.Errorf("status = %s, want failed (credentialed pushes cannot be recovered)", got.Status)
	}
	if got.Error == nil || !strings.Contains(*got.Error, "credentials") {
		t.Errorf("error = %v, want an explanation about borrowed credentials", got.Error)
	}

	// Nothing was pushed to the destination.
	dstRef, err := name.ParseReference(host+"/export/recovered:v1", name.Insecure)
	if err != nil {
		t.Fatalf("parse target: %v", err)
	}
	if _, err := remote.Get(dstRef, remote.WithAuth(authn.Anonymous)); err == nil {
		t.Error("recovered credentialed push should not have pushed")
	}
}

// Ensure ocicache errors surface through the manager when blobs disappear.
func TestCreatePushMissingBlobs(t *testing.T) {
	p, digest := cacheFixture(t)
	host := openRegistry(t)
	resolver := &fakeResolver{images: map[string]*images.Image{
		"myapp:v1": readyImage("myapp:v1", digest),
	}}
	mgr, err := NewManager(p, resolver, nil, 1)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	if err := os.RemoveAll(p.OCICacheBlobDir()); err != nil {
		t.Fatalf("remove blobs: %v", err)
	}

	push, err := mgr.CreatePush(context.Background(), PushRequest{
		Image: "myapp:v1", Target: host + "/export/app:v1", Insecure: true,
	})
	if err != nil {
		t.Fatalf("CreatePush: %v", err)
	}
	got := waitTerminal(t, mgr, push.ID)
	if got.Status != StatusFailed {
		t.Fatalf("status = %s, want failed (error: %v)", got.Status, got.Error)
	}
	if got.Error == nil || !strings.Contains(*got.Error, ocicache.ErrNotFound.Error()) {
		t.Errorf("error = %v, want ocicache.ErrNotFound", got.Error)
	}
}

// A push interrupted before a restart whose blobs were reclaimed by GC in the
// meantime fails the same way on recovery instead of hanging or wedging the slot.
func TestRecoveryFailsWhenBlobsReclaimed(t *testing.T) {
	p, digest := cacheFixture(t)
	host := openRegistry(t)

	meta := &pushMetadata{
		ID:        "recovered-missing-blobs",
		Status:    StatusPushing,
		Image:     "myapp:v1",
		Digest:    digest,
		Target:    host + "/export/recovered:v1",
		Insecure:  true,
		CreatedAt: time.Now(),
	}
	writePushes(t, p, meta)
	if err := os.RemoveAll(p.OCICacheBlobDir()); err != nil {
		t.Fatalf("remove blobs: %v", err)
	}

	mgr, err := NewManager(p, &fakeResolver{images: map[string]*images.Image{
		"myapp:v1": readyImage("myapp:v1", digest),
	}}, nil, 1)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	got := waitTerminal(t, mgr, "recovered-missing-blobs")
	if got.Status != StatusFailed {
		t.Fatalf("status = %s, want failed (error: %v)", got.Status, got.Error)
	}
	if got.Error == nil || !strings.Contains(*got.Error, ocicache.ErrNotFound.Error()) {
		t.Errorf("error = %v, want ocicache.ErrNotFound", got.Error)
	}
}

// panickingProvider blows up during credential resolution, standing in for a
// panic anywhere in the push path.
type panickingProvider struct{}

func (panickingProvider) Authenticator(_ context.Context, _ name.Reference) (authn.Authenticator, error) {
	panic("boom")
}

func TestExecutePushContainsPanic(t *testing.T) {
	mgr, _ := testManager(t, 1, panickingProvider{}, nil)
	host := openRegistry(t)

	push, err := mgr.CreatePush(context.Background(), PushRequest{
		Image: "myapp:v1", Target: host + "/export/panic:v1", Insecure: true,
	})
	if err != nil {
		t.Fatalf("CreatePush: %v", err)
	}

	got := waitTerminal(t, mgr, push.ID)
	if got.Status != StatusFailed {
		t.Errorf("status = %s, want failed", got.Status)
	}
	if got.Error == nil || !strings.Contains(*got.Error, "panicked") {
		t.Errorf("error = %v, want panic explanation", got.Error)
	}
}

func TestRecoveryTreatsInsecureAsDistinctKey(t *testing.T) {
	p, digest := cacheFixture(t)
	host := openRegistry(t)
	target := host + "/export/distinct:v1"
	now := time.Now()

	secure := &pushMetadata{ID: "secure", Status: StatusQueued, Image: "myapp:v1", Digest: digest, Target: target, Insecure: false, CreatedAt: now.Add(-time.Minute)}
	insecure := &pushMetadata{ID: "insecure", Status: StatusQueued, Image: "myapp:v1", Digest: digest, Target: target, Insecure: true, CreatedAt: now}
	writePushes(t, p, secure, insecure)

	resolver := &fakeResolver{images: map[string]*images.Image{
		"myapp:v1": readyImage("myapp:v1", digest),
	}}
	mgr, err := NewManager(p, resolver, nil, 1)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	// Same digest+target with different transport modes is distinct work:
	// both jobs recover, neither is superseded. Both pushes still go over
	// plain HTTP here: go-containerregistry's Registry.Scheme maps loopback
	// and RFC1918 hosts to http on its own, so an httptest registry cannot
	// exercise the secure transport — what this pins down is the key
	// distinction, not the wire behavior.
	for _, id := range []string{"secure", "insecure"} {
		mustPushed(t, mgr, id)
	}
}

func TestRecoverySweepsOrphanDirs(t *testing.T) {
	p, digest := cacheFixture(t)
	host := openRegistry(t)

	meta := &pushMetadata{ID: "real", Status: StatusQueued, Image: "myapp:v1", Digest: digest, Target: host + "/export/real:v1", Insecure: true, CreatedAt: time.Now()}
	writePushes(t, p, meta)
	// A crash between the push-dir MkdirAll and the metadata rename leaves an
	// empty dir; startup recovery sweeps it since it holds no record.
	if err := os.MkdirAll(p.PushDir("orphan"), 0755); err != nil {
		t.Fatalf("mkdir orphan: %v", err)
	}

	resolver := &fakeResolver{images: map[string]*images.Image{
		"myapp:v1": readyImage("myapp:v1", digest),
	}}
	mgr, err := NewManager(p, resolver, nil, 1)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	if _, err := os.Stat(p.PushDir("orphan")); !os.IsNotExist(err) {
		t.Errorf("orphan dir still exists after recovery (stat err = %v)", err)
	}
	mustPushed(t, mgr, "real")
}

func TestCredFingerprintNormalizesAuth(t *testing.T) {
	// The same login supplied as Username/Password and as the precomputed
	// base64 "user:pass" Auth shorthand must hash identically, so in-flight
	// dedup does not report a false credential conflict between the two forms.
	basic := &authn.AuthConfig{Username: "pusher", Password: "hunter2"}
	shorthand := &authn.AuthConfig{
		Auth: base64.StdEncoding.EncodeToString([]byte("pusher:hunter2")),
	}

	basicFp := credFingerprint(basic)
	if basicFp == "" {
		t.Fatal("basic-auth fingerprint should be non-empty")
	}
	shorthandFp := credFingerprint(shorthand)
	if shorthandFp != basicFp {
		t.Errorf("Auth-shorthand fingerprint %q != basic %q", shorthandFp, basicFp)
	}
	if credFingerprint(nil) != "" || credFingerprint(&authn.AuthConfig{}) != "" {
		t.Error("anonymous configs should share the empty fingerprint")
	}
}
