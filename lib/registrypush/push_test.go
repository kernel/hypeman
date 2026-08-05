package registrypush

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/kernel/hypeman/lib/images"
	"github.com/kernel/hypeman/lib/ocicache"
	"github.com/kernel/hypeman/lib/paths"
)

// newTestRegistry serves an in-process OCI registry over plain HTTP.
func newTestRegistry(t *testing.T) (host string, handler http.Handler) {
	t.Helper()
	handler = registry.New()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return strings.TrimPrefix(srv.URL, "http://"), handler
}

func startServer(t *testing.T, handler http.Handler) string {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return strings.TrimPrefix(srv.URL, "http://")
}

func cacheFixture(t *testing.T) (*paths.Paths, string) {
	t.Helper()
	p := paths.New(t.TempDir())
	img, err := random.Image(256, 2)
	if err != nil {
		t.Fatalf("random image: %v", err)
	}
	digest, err := writeToCache(p, img)
	if err != nil {
		t.Fatalf("write cache: %v", err)
	}
	return p, digest
}

func TestPushFromCache(t *testing.T) {
	host, _ := newTestRegistry(t)
	p, digest := cacheFixture(t)

	target := host + "/test/app:v1"
	result, err := PushFromCache(context.Background(), p, digest, target, nil, Options{Insecure: true})
	if err != nil {
		t.Fatalf("PushFromCache: %v", err)
	}

	// The pushed digest must match the cached image (Docker v2 input is
	// converted to OCI, so it may differ from the source image's digest).
	cached, err := ocicache.ImageFromCache(p, digest)
	if err != nil {
		t.Fatalf("read cached image: %v", err)
	}
	wantDigest, err := cached.Digest()
	if err != nil {
		t.Fatalf("cached digest: %v", err)
	}
	if result.Digest != wantDigest.String() {
		t.Errorf("result digest = %s, want %s", result.Digest, wantDigest)
	}
	if result.Layers != 2 {
		t.Errorf("result layers = %d, want 2", result.Layers)
	}
	if result.Bytes <= 0 {
		t.Errorf("result bytes = %d, want > 0", result.Bytes)
	}

	// Read the pushed image back from the destination registry.
	dstRef, err := name.ParseReference(target, name.Insecure)
	if err != nil {
		t.Fatalf("parse target: %v", err)
	}
	desc, err := remote.Get(dstRef, remote.WithAuth(authn.Anonymous))
	if err != nil {
		t.Fatalf("read back pushed image: %v", err)
	}
	if desc.Digest.String() != wantDigest.String() {
		t.Errorf("pushed digest = %s, want %s", desc.Digest, wantDigest)
	}
}

func TestPushFromCacheNotFound(t *testing.T) {
	p := paths.New(t.TempDir())

	_, err := PushFromCache(context.Background(), p, "sha256:"+strings.Repeat("0", 64),
		"localhost:1/app:v1", nil, Options{Insecure: true})
	if !errors.Is(err, ocicache.ErrNotFound) {
		t.Errorf("err = %v, want ocicache.ErrNotFound", err)
	}
}

func TestPushInvalidTarget(t *testing.T) {
	p, digest := cacheFixture(t)

	_, err := PushFromCache(context.Background(), p, digest, "!!!invalid", nil, Options{Insecure: true})
	if err == nil {
		t.Fatal("expected error for invalid target reference")
	}
}

func TestPushBearerAuth(t *testing.T) {
	_, registryHandler := newTestRegistry(t)
	gated := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer secret-token" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		registryHandler.ServeHTTP(w, r)
	})
	host := startServer(t, gated)

	p, digest := cacheFixture(t)
	target := host + "/test/app:v1"
	provider := &StaticProvider{Config: authn.AuthConfig{RegistryToken: "secret-token"}}

	if _, err := PushFromCache(context.Background(), p, digest, target, provider, Options{Insecure: true}); err != nil {
		t.Fatalf("push with valid bearer token: %v", err)
	}

	bad := &StaticProvider{Config: authn.AuthConfig{RegistryToken: "wrong"}}
	if _, err := PushFromCache(context.Background(), p, digest, target, bad, Options{Insecure: true}); err == nil {
		t.Fatal("expected error pushing with wrong bearer token")
	}
}

func TestPushRateLimited(t *testing.T) {
	host := startServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		w.Write([]byte(`{"errors":[{"code":"TOOMANYREQUESTS","message":"rate limit exceeded"}]}`))
	}))

	p, digest := cacheFixture(t)
	_, err := PushFromCache(context.Background(), p, digest, host+"/test/app:v1", nil, Options{Insecure: true})
	if !errors.Is(err, images.ErrRateLimited) {
		t.Errorf("err = %v, want images.ErrRateLimited", err)
	}
}

func TestPushRepoNotFound(t *testing.T) {
	host := startServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(map[string]any{
			"errors": []map[string]string{{"code": "NAME_UNKNOWN", "message": "repository name not known"}},
		})
	}))

	p, digest := cacheFixture(t)
	_, err := PushFromCache(context.Background(), p, digest, host+"/missing/app:v1", nil, Options{Insecure: true})
	if !errors.Is(err, images.ErrNotFound) {
		t.Errorf("err = %v, want images.ErrNotFound", err)
	}
}
