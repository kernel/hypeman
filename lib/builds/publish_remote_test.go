package builds

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPublishedRepository(t *testing.T) {
	cfg := PublishConfig{Registry: "registry.example.com", RepositoryPrefix: "team/builds"}

	tests := []struct {
		name     string
		cfg      PublishConfig
		localRef string
		want     string
	}{
		{"build repo", cfg, "builds/abc123", "registry.example.com/team/builds/abc123"},
		{"image name", cfg, "myapp", "registry.example.com/team/builds/myapp"},
		{"nested repo uses last segment", cfg, "tenant/team/myapp", "registry.example.com/team/builds/myapp"},
		{"tag stripped", cfg, "builds/abc123:latest", "registry.example.com/team/builds/abc123"},
		{"digest stripped", cfg, "builds/abc123@sha256:abc", "registry.example.com/team/builds/abc123"},
		{
			"trailing slashes trimmed",
			PublishConfig{Registry: "registry.example.com/", RepositoryPrefix: "/team/builds/"},
			"builds/abc123",
			"registry.example.com/team/builds/abc123",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, publishedRepository(tt.cfg, tt.localRef))
		})
	}
}

// newFakeRegistry starts an in-memory OCI registry and returns its host.
func newFakeRegistry(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(registry.New())
	t.Cleanup(srv.Close)
	return strings.TrimPrefix(srv.URL, "http://")
}

// pushRandomImage pushes a small random image to the registry and returns its digest.
func pushRandomImage(t *testing.T, ref string) string {
	t.Helper()
	img, err := random.Image(256, 2)
	require.NoError(t, err)
	r, err := name.ParseReference(ref)
	require.NoError(t, err)
	require.NoError(t, remote.Write(r, img))
	d, err := img.Digest()
	require.NoError(t, err)
	return d.String()
}

func newTestRemotePublisher(t *testing.T, cfg PublishConfig, localRegistry string) BuildPublisher {
	t.Helper()
	p, err := newRemotePublisher(cfg, localRegistry, NewRegistryTokenGenerator("test-secret"))
	require.NoError(t, err)
	return p
}

func TestRemotePublisher_PublishesAndVerifies(t *testing.T) {
	srcHost := newFakeRegistry(t)
	dstHost := newFakeRegistry(t)

	digest := pushRandomImage(t, srcHost+"/builds/build-1:latest")

	p := newTestRemotePublisher(t, PublishConfig{
		Registry:         dstHost,
		RepositoryPrefix: "team/builds",
	}, srcHost)

	ref, err := p.Publish(context.Background(), "builds/build-1", digest)
	require.NoError(t, err)
	assert.Equal(t, fmt.Sprintf("%s/team/builds/build-1@%s", dstHost, digest), ref)

	// The remote manifest is reachable by digest
	dstRef, err := name.NewDigest(ref)
	require.NoError(t, err)
	head, err := remote.Head(dstRef)
	require.NoError(t, err)
	assert.Equal(t, digest, head.Digest.String())
}

func TestRemotePublisher_IdempotentRetrySafe(t *testing.T) {
	srcHost := newFakeRegistry(t)
	dstHost := newFakeRegistry(t)

	digest := pushRandomImage(t, srcHost+"/builds/build-2:latest")

	p := newTestRemotePublisher(t, PublishConfig{
		Registry:         dstHost,
		RepositoryPrefix: "team/builds",
	}, srcHost)

	// Publishing the same image twice must succeed and return the same ref —
	// blob and manifest uploads are idempotent.
	ref1, err := p.Publish(context.Background(), "builds/build-2", digest)
	require.NoError(t, err)
	ref2, err := p.Publish(context.Background(), "builds/build-2", digest)
	require.NoError(t, err)
	assert.Equal(t, ref1, ref2)
}

func TestRemotePublisher_RetriesTransientFailures(t *testing.T) {
	srcHost := newFakeRegistry(t)
	digest := pushRandomImage(t, srcHost+"/builds/build-3:latest")

	// Destination registry that fails the first two blob uploads with a 500,
	// then proxies to the real in-memory registry.
	var failures atomic.Int32
	handler := registry.New()
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if failures.Add(1) <= 2 && strings.Contains(r.URL.Path, "/blobs/") {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		handler.ServeHTTP(w, r)
	}))
	t.Cleanup(backend.Close)
	dstHost := strings.TrimPrefix(backend.URL, "http://")

	p := newTestRemotePublisher(t, PublishConfig{
		Registry:         dstHost,
		RepositoryPrefix: "team/builds",
	}, srcHost)

	ref, err := p.Publish(context.Background(), "builds/build-3", digest)
	require.NoError(t, err)
	assert.Equal(t, fmt.Sprintf("%s/team/builds/build-3@%s", dstHost, digest), ref)
}

func TestRemotePublisher_DoesNotRetryPermanentFailures(t *testing.T) {
	srcHost := newFakeRegistry(t)
	digest := pushRandomImage(t, srcHost+"/builds/build-4:latest")

	// Destination registry that always rejects blob uploads with a 401.
	// remote.Write pings /v2/ once per call, so counting pings counts
	// pushWithRetry attempts: a permanent error must not be retried.
	var attempts atomic.Int32
	handler := registry.New()
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/v2/" {
			attempts.Add(1)
		}
		if strings.Contains(r.URL.Path, "/blobs/uploads/") {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		handler.ServeHTTP(w, r)
	}))
	t.Cleanup(backend.Close)
	dstHost := strings.TrimPrefix(backend.URL, "http://")

	p := newTestRemotePublisher(t, PublishConfig{
		Registry:         dstHost,
		RepositoryPrefix: "team/builds",
	}, srcHost)

	_, err := p.Publish(context.Background(), "builds/build-4", digest)
	require.Error(t, err)
	assert.EqualValues(t, 1, attempts.Load(), "permanent errors must not be retried")
}

func TestVerifyRemoteDigest_Mismatch(t *testing.T) {
	dstHost := newFakeRegistry(t)
	digest := pushRandomImage(t, dstHost+"/team/builds/build-5:latest")
	otherDigest := pushRandomImage(t, dstHost+"/team/builds/other:latest")

	ref, err := name.NewDigest(fmt.Sprintf("%s/team/builds/build-5@%s", dstHost, digest))
	require.NoError(t, err)

	err = verifyRemoteDigest(context.Background(), ref, otherDigest, anonymousKeychain{})
	require.ErrorContains(t, err, "digest mismatch")
}

func TestVerifyRemoteDigest_NotFound(t *testing.T) {
	dstHost := newFakeRegistry(t)
	missing := fmt.Sprintf("%s/team/builds/never-pushed@sha256:%s", dstHost, strings.Repeat("0", 64))

	ref, err := name.NewDigest(missing)
	require.NoError(t, err)

	err = verifyRemoteDigest(context.Background(), ref, "sha256:"+strings.Repeat("0", 64), anonymousKeychain{})
	require.ErrorContains(t, err, "verify remote manifest")
}

func TestRemotePublisher_UnreachableRegistry(t *testing.T) {
	srcHost := newFakeRegistry(t)
	digest := pushRandomImage(t, srcHost+"/builds/build-6:latest")

	// Point the destination at a server that is already closed.
	dead := httptest.NewServer(registry.New())
	dstHost := strings.TrimPrefix(dead.URL, "http://")
	dead.Close()

	p := newTestRemotePublisher(t, PublishConfig{
		Registry:         dstHost,
		RepositoryPrefix: "team/builds",
	}, srcHost)

	_, err := p.Publish(context.Background(), "builds/build-6", digest)
	require.ErrorContains(t, err, "push to remote registry")
}

func TestRemotePublisher_AuthenticatesWithCredentialsFile(t *testing.T) {
	srcHost := newFakeRegistry(t)
	digest := pushRandomImage(t, srcHost+"/builds/build-7:latest")

	// Destination registry that requires basic auth.
	const user, pass = "builder-bot", "s3cr3t-token"
	var authenticated atomic.Bool
	handler := registry.New()
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, p, ok := r.BasicAuth()
		if !ok || u != user || p != pass {
			w.Header().Set("WWW-Authenticate", `Basic realm="registry"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		authenticated.Store(true)
		handler.ServeHTTP(w, r)
	}))
	t.Cleanup(backend.Close)
	dstHost := strings.TrimPrefix(backend.URL, "http://")

	credFile := filepath.Join(t.TempDir(), "config.json")
	auth := base64.StdEncoding.EncodeToString([]byte(user + ":" + pass))
	require.NoError(t, os.WriteFile(credFile, []byte(fmt.Sprintf(
		`{"auths":{"http://%s/":{"auth":%q}}}`, dstHost, auth)), 0600))

	p, err := newRemotePublisher(PublishConfig{
		Registry:         dstHost,
		RepositoryPrefix: "team/builds",
		CredentialsFile:  credFile,
	}, srcHost, NewRegistryTokenGenerator("test-secret"))
	require.NoError(t, err)

	ref, err := p.Publish(context.Background(), "builds/build-7", digest)
	require.NoError(t, err)
	assert.Equal(t, fmt.Sprintf("%s/team/builds/build-7@%s", dstHost, digest), ref)
	assert.True(t, authenticated.Load(), "expected the remote registry to see the credentials")
}

func TestDockerFileKeychain(t *testing.T) {
	t.Run("empty path is anonymous", func(t *testing.T) {
		k, err := dockerFileKeychain("")
		require.NoError(t, err)
		auth, err := k.Resolve(mustRegistry(t, "registry.example.com"))
		require.NoError(t, err)
		assert.Equal(t, authn.Anonymous, auth)
	})

	t.Run("missing file", func(t *testing.T) {
		_, err := dockerFileKeychain(filepath.Join(t.TempDir(), "nope.json"))
		require.ErrorContains(t, err, "read credentials file")
	})

	t.Run("malformed json", func(t *testing.T) {
		f := filepath.Join(t.TempDir(), "config.json")
		require.NoError(t, os.WriteFile(f, []byte("{not json"), 0600))
		_, err := dockerFileKeychain(f)
		require.ErrorContains(t, err, "parse credentials file")
	})

	t.Run("resolves credentials by registry host", func(t *testing.T) {
		f := filepath.Join(t.TempDir(), "config.json")
		auth := base64.StdEncoding.EncodeToString([]byte("user:pass"))
		require.NoError(t, os.WriteFile(f, []byte(fmt.Sprintf(
			`{"auths":{"https://registry.example.com/":{"auth":%q}}}`, auth)), 0600))

		k, err := dockerFileKeychain(f)
		require.NoError(t, err)

		authenticator, err := k.Resolve(mustRegistry(t, "registry.example.com"))
		require.NoError(t, err)
		cfg, err := authenticator.Authorization()
		require.NoError(t, err)
		assert.Equal(t, "user", cfg.Username)
		assert.Equal(t, "pass", cfg.Password)

		// Unknown registries fall back to anonymous
		other, err := k.Resolve(mustRegistry(t, "other.example.com"))
		require.NoError(t, err)
		assert.Equal(t, authn.Anonymous, other)
	})
}

func mustRegistry(t *testing.T, host string) name.Registry {
	t.Helper()
	r, err := name.NewRegistry(host)
	require.NoError(t, err)
	return r
}

// stubPublisher returns a fixed result for manager-level publish tests.
type stubPublisher struct {
	ref string
	err error
}

func (s stubPublisher) Publish(_ context.Context, _, _ string) (string, error) {
	return s.ref, s.err
}

// A configured publication failure fails the build, even though the build and
// local image conversion succeeded.
func TestRunBuild_PublishFailure_FailsBuild(t *testing.T) {
	mgr, instanceMgr, _, imageMgr, tempDir := setupTestManagerWithImageMgr(t)
	defer os.RemoveAll(tempDir)

	mgr.publisher = stubPublisher{err: errors.New("remote registry unreachable")}

	digest := "sha256:" + strings.Repeat("cd", 32)
	build := runBuildToCompletion(t, mgr, instanceMgr, imageMgr, "build-pub-fails", &BuildResult{
		Success:     true,
		ImageDigest: digest,
	})

	assert.Equal(t, StatusFailed, build.Status)
	require.NotNil(t, build.Error)
	assert.Contains(t, *build.Error, "image publication failed")
	assert.Contains(t, *build.Error, "remote registry unreachable")
	assert.Nil(t, build.ImageRef)
}

// With publication configured, the build records the immutable remote
// reference returned by the publisher.
func TestRunBuild_PublishConfigured_RecordsRemoteRef(t *testing.T) {
	mgr, instanceMgr, _, imageMgr, tempDir := setupTestManagerWithImageMgr(t)
	defer os.RemoveAll(tempDir)

	digest := "sha256:" + strings.Repeat("ef", 32)
	remoteRef := "registry.example.com/team/builds/build-pub-ok@" + digest
	mgr.publisher = stubPublisher{ref: remoteRef}

	build := runBuildToCompletion(t, mgr, instanceMgr, imageMgr, "build-pub-ok", &BuildResult{
		Success:     true,
		ImageDigest: digest,
	})

	assert.Equal(t, StatusReady, build.Status)
	require.NotNil(t, build.ImageRef)
	assert.Equal(t, remoteRef, *build.ImageRef)
}

// Publish credentials stay on the host: the build config written for the
// builder VM must never contain publish settings or credential material.
func TestCreateBuild_ConfigNeverContainsPublishCredentials(t *testing.T) {
	mgr, _, _, tempDir := setupTestManager(t)
	defer os.RemoveAll(tempDir)

	credFile := filepath.Join(tempDir, "creds.json")
	require.NoError(t, os.WriteFile(credFile, []byte(
		`{"auths":{"registry.example.com":{"auth":"dXNlcjpwYXNz"}}}`), 0600))
	mgr.config.Publish = PublishConfig{
		Registry:         "registry.example.com",
		RepositoryPrefix: "team/builds",
		CredentialsFile:  credFile,
	}

	build, err := mgr.CreateBuild(context.Background(), CreateBuildRequest{
		Dockerfile: "FROM alpine",
	}, []byte("fake-tarball-data"))
	require.NoError(t, err)

	data, err := os.ReadFile(mgr.paths.BuildConfig(build.ID))
	require.NoError(t, err)
	assert.NotContains(t, string(data), "registry.example.com")
	assert.NotContains(t, string(data), credFile)
	assert.NotContains(t, string(data), "dXNlcjpwYXNz")
}
