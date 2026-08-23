package images

import (
	"context"
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
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/layout"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/kernel/hypeman/lib/paths"
	"github.com/kernel/hypeman/lib/queue"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBorrowedCredentialsAuthenticateResolveAndPull(t *testing.T) {
	const username = "borrower"
	const password = "pull-secret"

	handler := registry.New()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUser, gotPassword, ok := r.BasicAuth()
		if !ok || gotUser != username || gotPassword != password {
			w.Header().Set("WWW-Authenticate", `Basic realm="registry"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		handler.ServeHTTP(w, r)
	}))
	defer server.Close()

	registryHost := strings.TrimPrefix(server.URL, "http://")
	ref, err := name.ParseReference(registryHost+"/private/image:latest", name.Insecure)
	require.NoError(t, err)
	credentials := &authn.AuthConfig{Username: username, Password: password}

	img, err := random.Image(256, 1)
	require.NoError(t, err)
	require.NoError(t, remote.Write(ref, img, remote.WithAuth(authn.FromConfig(*credentials))))

	client, err := newOCIClient(t.TempDir())
	require.NoError(t, err)
	platform := vmPlatform()
	digest, err := client.inspectManifestWithPlatformAuth(context.Background(), ref.String(), platform, credentials)
	require.NoError(t, err)

	err = client.pullToOCILayoutWithPlatformAuth(context.Background(), ref.Context().Digest(digest).String(), digestToLayoutTag(digest), platform, credentials)
	require.NoError(t, err)
	assert.True(t, client.existsInLayout(digestToLayoutTag(digest)))
}

func TestCreateImageRequestCredentialsAreNotPersisted(t *testing.T) {
	const secret = "must-not-reach-disk"
	const repository = "registry.example/private/image"
	const digest = "abc"
	p := paths.New(t.TempDir())
	meta := imageMetadata{
		Name:         repository + ":latest",
		Digest:       "sha256:" + digest,
		Status:       StatusPending,
		BorrowedAuth: true,
		Request: &CreateImageRequest{
			Name:        repository + ":latest",
			Credentials: &authn.AuthConfig{Username: "borrower", Password: secret},
		},
	}

	require.NoError(t, writeMetadata(p, repository, digest, &meta))
	data, err := os.ReadFile(metadataPath(p, repository, digest))
	require.NoError(t, err)
	assert.NotContains(t, string(data), secret)
	assert.NotContains(t, string(data), "borrower")
	assert.Contains(t, string(data), `"borrowed_auth": true`)
}

func TestInflightPullRejectsDifferentCredentials(t *testing.T) {
	m := &manager{
		inflightPulls:              make(map[string]*inflightImagePull),
		borrowedCredentialsTimeout: time.Minute,
	}
	const digest = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	credentials := &authn.AuthConfig{Username: "AWS", Password: "token-a"}
	inflight := m.registerInflightPull(digest, credentials)
	t.Cleanup(m.releaseInflightPull(digest, inflight))

	assert.True(t, m.inflightCredentialsMatch(digest, &authn.AuthConfig{Username: "AWS", Password: "token-a"}))
	assert.False(t, m.inflightCredentialsMatch(digest, nil))
	assert.False(t, m.inflightCredentialsMatch(digest, &authn.AuthConfig{Username: "AWS", Password: "token-b"}))
}

func TestBorrowedCredentialsExpireWhileQueued(t *testing.T) {
	m := &manager{
		inflightPulls:              make(map[string]*inflightImagePull),
		borrowedCredentialsTimeout: time.Millisecond,
	}
	const digest = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	inflight := m.registerInflightPull(digest, &authn.AuthConfig{Username: "AWS", Password: "secret"})
	defer m.releaseInflightPull(digest, inflight)()

	require.Eventually(t, func() bool {
		m.createMu.Lock()
		defer m.createMu.Unlock()
		return m.inflightPulls[digest].credentials == nil
	}, time.Second, time.Millisecond)

	credentials, _, expired, stale := m.borrowedAuth(digest, inflight)
	assert.True(t, expired)
	assert.False(t, stale)
	assert.Nil(t, credentials)
}

func TestBorrowedAuthRejectsReplacedInflightPull(t *testing.T) {
	m := &manager{inflightPulls: make(map[string]*inflightImagePull)}
	const digest = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	first := m.registerInflightPull(digest, &authn.AuthConfig{Username: "first"})
	second := m.registerInflightPull(digest, &authn.AuthConfig{Username: "second"})
	defer m.releaseInflightPull(digest, second)()

	credentials, _, expired, stale := m.borrowedAuth(digest, first)
	assert.Nil(t, credentials)
	assert.False(t, expired)
	assert.True(t, stale)

	credentials, _, expired, stale = m.borrowedAuth(digest, second)
	assert.Equal(t, "second", credentials.Username)
	assert.False(t, expired)
	assert.False(t, stale)
}

func TestRecoverInterruptedCredentialedConversionFromCache(t *testing.T) {
	p := paths.New(t.TempDir())
	img := createTestDockerImage(t)
	digest, err := img.Digest()
	require.NoError(t, err)
	digestString := digest.String()

	require.NoError(t, os.MkdirAll(p.SystemOCICache(), 0o755))
	cache, err := layout.Write(p.SystemOCICache(), empty.Index)
	require.NoError(t, err)
	require.NoError(t, cache.AppendImage(img, layout.WithAnnotations(map[string]string{
		"org.opencontainers.image.ref.name": digestToLayoutTag(digestString),
	})))

	const repository = "registry.example/private/image"
	meta := &imageMetadata{
		Name:         repository + ":latest",
		Digest:       digestString,
		Status:       StatusConverting,
		BorrowedAuth: true,
		Request:      &CreateImageRequest{Name: repository + ":latest"},
		CreatedAt:    time.Now(),
	}
	require.NoError(t, writeMetadata(p, repository, strings.TrimPrefix(digestString, "sha256:"), meta))

	mgr, err := NewManager(p, 1, nil)
	require.NoError(t, err)
	waitForReady(t, mgr, context.Background(), meta.Name)

	recovered, err := mgr.GetImage(context.Background(), meta.Name)
	require.NoError(t, err)
	assert.Equal(t, StatusReady, recovered.Status)
}

func TestRecoverInterruptedCredentialedPullFailsForFreshRetry(t *testing.T) {
	p := paths.New(t.TempDir())
	client, err := newOCIClient(p.SystemOCICache())
	require.NoError(t, err)
	m := &manager{
		paths:            p,
		ociClient:        client,
		queue:            queue.New(1),
		readySubscribers: make(map[string][]chan StatusEvent),
	}

	const repository = "registry.example/private/image"
	const digest = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	meta := &imageMetadata{
		Name:         repository + ":latest",
		Digest:       digest,
		Status:       StatusPulling,
		BorrowedAuth: true,
		Request:      &CreateImageRequest{Name: repository + ":latest"},
		CreatedAt:    time.Now(),
	}
	require.NoError(t, writeMetadata(p, repository, strings.TrimPrefix(digest, "sha256:"), meta))

	m.RecoverInterruptedBuilds()

	stored, err := readMetadata(p, repository, strings.TrimPrefix(digest, "sha256:"))
	require.NoError(t, err)
	require.NotNil(t, stored.Error)
	assert.Equal(t, StatusFailed, stored.Status)
	assert.Equal(t, ErrBorrowedCredentialsExpired.Error(), *stored.Error)
	assert.Zero(t, m.queue.QueueLength())

	data, err := os.ReadFile(filepath.Join(p.ImageDigestDir(repository, strings.TrimPrefix(digest, "sha256:")), "metadata.json"))
	require.NoError(t, err)
	assert.NotContains(t, string(data), "password")
}
