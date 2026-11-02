package builds_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Code-Hex/vz/v4/diskimagekit"
	"github.com/kernel/hypeman/lib/builds"
	"github.com/kernel/hypeman/lib/images"
	"github.com/kernel/hypeman/lib/middleware"
	"github.com/kernel/hypeman/lib/paths"
	"github.com/kernel/hypeman/lib/registry"
	"github.com/stretchr/testify/require"
)

type machineImportStub struct {
	images.Manager
	imported chan string
}

func (m *machineImportStub) ImportLocalImage(_ context.Context, repo, reference, digest string) (*images.Image, error) {
	m.imported <- repo
	return &images.Image{Status: images.StatusReady, Digest: digest}, nil
}

func machinePublisherPlatform() images.MacOSImage {
	return images.MacOSImage{HardwareModel: []byte{1}, MachineIdentifier: []byte{2}, MAC: "02:00:00:00:00:01", CPUs: 4, Memory: 8 << 30, GuestAgent: true}
}

// machinePublisherStorage writes a standalone ASIF disk and aux file, as a base
// bundle has.
func machinePublisherStorage(t *testing.T) builds.MachineStorage {
	t.Helper()
	root := t.TempDir()
	storage := builds.MachineStorage{Overlay: filepath.Join(root, "disk.asif"), Aux: filepath.Join(root, "aux.img"), Platform: machinePublisherPlatform()}
	img, e := diskimagekit.CreateASIF(storage.Overlay, (4<<20)/512)
	require.NoError(t, e)
	_, e = img.WriteAt([]byte("synthetic disk"), 0)
	require.NoError(t, e)
	require.NoError(t, img.Close())
	require.NoError(t, os.WriteFile(storage.Aux, []byte("synthetic aux"), 0600))
	return storage
}

func TestStreamingMachinePublisherAuthenticatedRegistry(t *testing.T) {
	// Tiny storage is intentional: this proves auth/streaming/commit ordering, not bootability.
	imports := &machineImportStub{imported: make(chan string, 1)}
	reg, e := registry.New(paths.New(t.TempDir()), imports)
	require.NoError(t, e)
	var uploads, verified atomic.Int32
	handler := middleware.JwtAuth("synthetic registry secret")(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.NotEqual(t, http.MethodPatch, r.Method, "uploads must use the disk-streaming monolithic endpoint")
		if r.Method == http.MethodPost {
			require.NotEmpty(t, r.URL.Query().Get("digest"))
			require.Greater(t, r.ContentLength, int64(0))
			uploads.Add(1)
		}
		if r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/blobs/") {
			verified.Add(1)
		}
		if r.Method == http.MethodPut {
			require.Equal(t, int32(2), uploads.Load(), "manifest must be last")
		}
		reg.Handler().ServeHTTP(w, r)
	}))
	server := httptest.NewServer(handler)
	defer server.Close()
	publisher := &builds.StreamingMachinePublisher{RegistryURL: server.URL, Tokens: builds.NewRegistryTokenGenerator("synthetic registry secret")}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	result, e := publisher.Publish(ctx, "qa-build", machinePublisherStorage(t), nil)
	require.NoError(t, e)
	require.Contains(t, result.Reference, "/builds/qa-build@sha256:")
	select {
	case repo := <-imports.imported:
		require.Equal(t, "builds/qa-build", repo)
	case <-ctx.Done():
		t.Fatal("manifest did not reach production conversion boundary")
	}
}

func TestStreamingMachinePublisherRejectsRedirectWithoutLeakingToken(t *testing.T) {
	var visited atomic.Bool
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { visited.Store(true) }))
	defer other.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL, http.StatusTemporaryRedirect)
	}))
	defer server.Close()
	publisher := &builds.StreamingMachinePublisher{RegistryURL: server.URL, Tokens: builds.NewRegistryTokenGenerator("synthetic secret")}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, e := publisher.Publish(ctx, "qa-build", machinePublisherStorage(t), nil)
	require.Error(t, e)
	require.False(t, visited.Load())
}

func TestStreamingMachinePublisherReusesBaseLayers(t *testing.T) {
	imports := &machineImportStub{imported: make(chan string, 2)}
	reg, e := registry.New(paths.New(t.TempDir()), imports)
	require.NoError(t, e)
	var uploads atomic.Int32
	server := httptest.NewServer(middleware.JwtAuth("synthetic registry secret")(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			uploads.Add(1)
		}
		reg.Handler().ServeHTTP(w, r)
	})))
	defer server.Close()
	publisher := &builds.StreamingMachinePublisher{RegistryURL: server.URL, Tokens: builds.NewRegistryTokenGenerator("synthetic registry secret")}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	baseStorage := machinePublisherStorage(t)
	base, e := publisher.Publish(ctx, "qa-base", baseStorage, nil)
	require.NoError(t, e)
	require.Equal(t, int32(2), uploads.Load(), "config and one layer")

	// A build stacks a writable overlay on the base disk.
	img, e := diskimagekit.Open(baseStorage.Overlay)
	require.NoError(t, e)
	derived := builds.MachineStorage{Layers: []string{baseStorage.Overlay}, Overlay: filepath.Join(t.TempDir(), "boot.asif"), Aux: filepath.Join(t.TempDir(), "aux.img"), Platform: machinePublisherPlatform()}
	require.NoError(t, img.CreateLayer(derived.Overlay, diskimagekit.Overlay))
	_, e = img.WriteAt([]byte("build output"), 1<<20)
	require.NoError(t, e)
	require.NoError(t, img.Close())
	require.NoError(t, os.WriteFile(derived.Aux, []byte("build aux"), 0600))
	derived.Platform.MachineIdentifier = []byte{3}

	uploads.Store(0)
	machineBase := &builds.MachineBase{Repo: "builds/qa-base", Digest: base.Digest}
	result, e := publisher.Publish(ctx, "qa-derived", derived, machineBase)
	require.NoError(t, e)
	require.Equal(t, int32(2), uploads.Load(), "only the config and the overlay layer are uploaded")
	require.NotEqual(t, base.Digest, result.Digest)

	derived.Layers = nil
	_, e = publisher.Publish(ctx, "qa-derived", derived, machineBase)
	require.ErrorContains(t, e, "machine base has 1 layers but the disk stacks on 0")
}
