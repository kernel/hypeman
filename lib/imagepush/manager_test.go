package imagepush

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
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

func TestCreatePushEndToEnd(t *testing.T) {
	p, digest := cacheFixture(t)
	host := openRegistry(t)
	resolver := &fakeResolver{images: map[string]*images.Image{
		"myapp:v1": readyImage("myapp:v1", digest),
	}}

	mgr, err := NewManager(p, resolver, nil, 2)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	target := host + "/export/app:v1"
	push, err := mgr.CreatePush(context.Background(), PushRequest{Image: "myapp:v1", Target: target, Insecure: true})
	if err != nil {
		t.Fatalf("CreatePush: %v", err)
	}
	if push.Status != StatusQueued && push.Status != StatusPushing {
		t.Errorf("initial status = %s, want queued or pushing", push.Status)
	}

	if err := mgr.WaitForPush(context.Background(), push.ID); err != nil {
		t.Fatalf("WaitForPush: %v", err)
	}

	got, err := mgr.GetPush(context.Background(), push.ID)
	if err != nil {
		t.Fatalf("GetPush: %v", err)
	}
	if got.Status != StatusPushed {
		t.Errorf("status = %s, want pushed (error: %v)", got.Status, got.Error)
	}
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
	if digests := mgr.InProgressDigests(); len(digests) != 0 {
		t.Errorf("InProgressDigests = %v, want empty", digests)
	}
}

func TestCreatePushDedupesInFlight(t *testing.T) {
	p, digest := cacheFixture(t)
	host, gate := gatedRegistry(t)
	resolver := &fakeResolver{images: map[string]*images.Image{
		"myapp:v1": readyImage("myapp:v1", digest),
	}}

	mgr, err := NewManager(p, resolver, nil, 2)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

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

	if digests := mgr.InProgressDigests(); len(digests) != 1 || digests[0] != digest {
		t.Errorf("InProgressDigests = %v, want [%s]", digests, digest)
	}

	close(gate)
	if err := mgr.WaitForPush(context.Background(), first.ID); err != nil {
		t.Fatalf("WaitForPush: %v", err)
	}
}

func TestCreatePushQueuesBehindConcurrencyLimit(t *testing.T) {
	p, digest := cacheFixture(t)
	host, gate := gatedRegistry(t)
	resolver := &fakeResolver{images: map[string]*images.Image{
		"myapp:v1": readyImage("myapp:v1", digest),
	}}

	mgr, err := NewManager(p, resolver, nil, 1)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

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

	close(gate)
	if err := mgr.WaitForPush(context.Background(), first.ID); err != nil {
		t.Fatalf("WaitForPush first: %v", err)
	}
	if err := mgr.WaitForPush(context.Background(), second.ID); err != nil {
		t.Fatalf("WaitForPush second: %v", err)
	}
}

func TestCreatePushImageNotReady(t *testing.T) {
	p, digest := cacheFixture(t)
	resolver := &fakeResolver{images: map[string]*images.Image{
		"myapp:v1": {Name: "myapp:v1", Digest: digest, Status: images.StatusConverting},
	}}

	mgr, err := NewManager(p, resolver, nil, 1)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	_, err = mgr.CreatePush(context.Background(), PushRequest{Image: "myapp:v1", Target: "registry.example.com/app:v1"})
	if !errors.Is(err, ErrImageNotReady) {
		t.Errorf("err = %v, want ErrImageNotReady", err)
	}
}

func TestCreatePushUnknownImage(t *testing.T) {
	p, _ := cacheFixture(t)
	resolver := &fakeResolver{images: map[string]*images.Image{}}

	mgr, err := NewManager(p, resolver, nil, 1)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	_, err = mgr.CreatePush(context.Background(), PushRequest{Image: "missing:v1", Target: "registry.example.com/app:v1"})
	if !errors.Is(err, images.ErrNotFound) {
		t.Errorf("err = %v, want images.ErrNotFound", err)
	}
}

func TestCreatePushInvalidTarget(t *testing.T) {
	p, digest := cacheFixture(t)
	resolver := &fakeResolver{images: map[string]*images.Image{
		"myapp:v1": readyImage("myapp:v1", digest),
	}}

	mgr, err := NewManager(p, resolver, nil, 1)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	_, err = mgr.CreatePush(context.Background(), PushRequest{Image: "myapp:v1", Target: "!!!invalid"})
	if !errors.Is(err, ErrInvalidTarget) {
		t.Errorf("err = %v, want ErrInvalidTarget", err)
	}

	// Nothing was persisted for the invalid request.
	pushes, err := mgr.ListPushes(context.Background())
	if err != nil {
		t.Fatalf("ListPushes: %v", err)
	}
	if len(pushes) != 0 {
		t.Errorf("len(pushes) = %d, want 0", len(pushes))
	}
}

func TestCreatePushFailureRecorded(t *testing.T) {
	p, digest := cacheFixture(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	host := strings.TrimPrefix(srv.URL, "http://")

	resolver := &fakeResolver{images: map[string]*images.Image{
		"myapp:v1": readyImage("myapp:v1", digest),
	}}
	mgr, err := NewManager(p, resolver, nil, 1)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	push, err := mgr.CreatePush(context.Background(), PushRequest{
		Image: "myapp:v1", Target: host + "/export/app:v1", Insecure: true,
	})
	if err != nil {
		t.Fatalf("CreatePush: %v", err)
	}

	err = mgr.WaitForPush(context.Background(), push.ID)
	if err == nil {
		t.Fatal("WaitForPush should fail for a failed push")
	}

	got, err := mgr.GetPush(context.Background(), push.ID)
	if err != nil {
		t.Fatalf("GetPush: %v", err)
	}
	if got.Status != StatusFailed {
		t.Errorf("status = %s, want failed", got.Status)
	}
	if got.Error == nil || *got.Error == "" {
		t.Error("error not recorded on failed push")
	}
}

func TestListPushesNewestFirst(t *testing.T) {
	p, digest := cacheFixture(t)
	host := openRegistry(t)
	resolver := &fakeResolver{images: map[string]*images.Image{
		"myapp:v1": readyImage("myapp:v1", digest),
	}}
	mgr, err := NewManager(p, resolver, nil, 2)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	first, err := mgr.CreatePush(context.Background(), PushRequest{
		Image: "myapp:v1", Target: host + "/export/a:v1", Insecure: true,
	})
	if err != nil {
		t.Fatalf("CreatePush a: %v", err)
	}
	if err := mgr.WaitForPush(context.Background(), first.ID); err != nil {
		t.Fatalf("WaitForPush a: %v", err)
	}

	second, err := mgr.CreatePush(context.Background(), PushRequest{
		Image: "myapp:v1", Target: host + "/export/b:v1", Insecure: true,
	})
	if err != nil {
		t.Fatalf("CreatePush b: %v", err)
	}
	if err := mgr.WaitForPush(context.Background(), second.ID); err != nil {
		t.Fatalf("WaitForPush b: %v", err)
	}

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

	if err := mgr.WaitForPush(context.Background(), "recovered-push"); err != nil {
		t.Fatalf("WaitForPush recovered: %v", err)
	}
	got, err := mgr.GetPush(context.Background(), "recovered-push")
	if err != nil {
		t.Fatalf("GetPush: %v", err)
	}
	if got.Status != StatusPushed {
		t.Errorf("status = %s, want pushed", got.Status)
	}
}

func TestWaitForPushNotFound(t *testing.T) {
	p, _ := cacheFixture(t)
	resolver := &fakeResolver{images: map[string]*images.Image{}}
	mgr, err := NewManager(p, resolver, nil, 1)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	err = mgr.WaitForPush(context.Background(), "missing")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
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
	err = mgr.WaitForPush(context.Background(), push.ID)
	if err == nil {
		t.Fatal("WaitForPush should fail when cache blobs are missing")
	}
	// Depending on timing the failure is observed via the live event (typed)
	// or via persisted metadata (string), so accept both forms.
	if !errors.Is(err, ocicache.ErrNotFound) && !strings.Contains(err.Error(), ocicache.ErrNotFound.Error()) {
		t.Errorf("err = %v, want ocicache.ErrNotFound", err)
	}
}
