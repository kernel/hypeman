package machineimage_test

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
	"github.com/kernel/hypeman/lib/images"
	"github.com/kernel/hypeman/lib/machineimage"
	"github.com/kernel/hypeman/lib/middleware"
	"github.com/kernel/hypeman/lib/paths"
	"github.com/kernel/hypeman/lib/registry"
	"github.com/kernel/hypeman/lib/registrytoken"
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

func testMachineIdentifier() ([]byte, error) { return []byte{9}, nil }

func machinePublisherPlatform() images.MacOSImage {
	return images.MacOSImage{HardwareModel: []byte{1}, MachineIdentifier: []byte{2}, MAC: "02:00:00:00:00:01", CPUs: 4, Memory: 8 << 30, GuestAgent: true}
}

// machinePublisherStorage writes a standalone ASIF disk and aux file, as a base
// bundle has.
func machinePublisherStorage(t *testing.T) machineimage.Storage {
	t.Helper()
	root := t.TempDir()
	storage := machineimage.Storage{Overlay: filepath.Join(root, "disk.asif"), Aux: filepath.Join(root, "aux.img"), Platform: machinePublisherPlatform()}
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
	var uploads atomic.Int32
	handler := middleware.JwtAuth("synthetic registry secret")(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut && strings.Contains(r.URL.Path, "/blobs/uploads/") {
			uploads.Add(1)
		}
		if r.Method == http.MethodPut && strings.Contains(r.URL.Path, "/manifests/") {
			require.Equal(t, int32(2), uploads.Load(), "manifest must be last")
		}
		reg.Handler().ServeHTTP(w, r)
	}))
	server := httptest.NewServer(handler)
	defer server.Close()
	publisher := &machineimage.Publisher{RegistryURL: server.URL, Tokens: registrytoken.NewGenerator("synthetic registry secret"), NewMachineIdentifier: testMachineIdentifier}
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
	publisher := &machineimage.Publisher{RegistryURL: server.URL, Tokens: registrytoken.NewGenerator("synthetic secret"), NewMachineIdentifier: testMachineIdentifier}
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
		if r.Method == http.MethodPut && strings.Contains(r.URL.Path, "/blobs/uploads/") {
			uploads.Add(1)
		}
		reg.Handler().ServeHTTP(w, r)
	})))
	defer server.Close()
	publisher := &machineimage.Publisher{RegistryURL: server.URL, Tokens: registrytoken.NewGenerator("synthetic registry secret"), NewMachineIdentifier: testMachineIdentifier}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	baseStorage := machinePublisherStorage(t)
	base, e := publisher.Publish(ctx, "qa-base", baseStorage, nil)
	require.NoError(t, e)
	require.Equal(t, int32(2), uploads.Load(), "config and one layer")

	// A build stacks a writable overlay on the base disk.
	img, e := diskimagekit.Open(baseStorage.Overlay)
	require.NoError(t, e)
	derived := machineimage.Storage{Layers: []string{baseStorage.Overlay}, Overlay: filepath.Join(t.TempDir(), "boot.asif"), Aux: filepath.Join(t.TempDir(), "aux.img"), Platform: machinePublisherPlatform()}
	require.NoError(t, img.CreateLayer(derived.Overlay, diskimagekit.Overlay))
	_, e = img.WriteAt([]byte("build output"), 1<<20)
	require.NoError(t, e)
	require.NoError(t, img.Close())
	require.NoError(t, os.WriteFile(derived.Aux, []byte("build aux"), 0600))
	derived.Platform.MachineIdentifier = []byte{3}

	uploads.Store(0)
	machineBase := &machineimage.Base{Repo: "builds/qa-base", Digest: base.Digest}
	result, e := publisher.Publish(ctx, "qa-derived", derived, machineBase)
	require.NoError(t, e)
	require.Equal(t, int32(2), uploads.Load(), "only the config and the overlay layer are uploaded")
	require.NotEqual(t, base.Digest, result.Digest)

	derived.Layers = nil
	_, e = publisher.Publish(ctx, "qa-derived", derived, machineBase)
	require.ErrorContains(t, e, "machine base has 1 layers but the disk stacks on 0")

	// The same number of layers, but stacked on a different base disk.
	other := machinePublisherStorage(t)
	img, e = diskimagekit.Open(other.Overlay)
	require.NoError(t, e)
	mismatched := machineimage.Storage{Layers: []string{other.Overlay}, Overlay: filepath.Join(t.TempDir(), "boot.asif"), Aux: derived.Aux, Platform: machinePublisherPlatform()}
	require.NoError(t, img.CreateLayer(mismatched.Overlay, diskimagekit.Overlay))
	require.NoError(t, img.Close())
	_, e = publisher.Publish(ctx, "qa-mismatched", mismatched, machineBase)
	require.ErrorContains(t, e, "not the layers the disk stacks on")
}

func TestMintedTokenPublishesOnlyItsImage(t *testing.T) {
	imports := &machineImportStub{imported: make(chan string, 2)}
	reg, e := registry.New(paths.New(t.TempDir()), imports)
	require.NoError(t, e)
	server := httptest.NewServer(middleware.JwtAuth("synthetic registry secret")(reg.Handler()))
	defer server.Close()
	token, e := machineimage.MintToken(registrytoken.NewGenerator("synthetic registry secret"), "qa-minted", nil, time.Hour)
	require.NoError(t, e)
	publisher := &machineimage.Publisher{RegistryURL: server.URL, Token: token, NewMachineIdentifier: testMachineIdentifier}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	_, e = publisher.Publish(ctx, "qa-minted", machinePublisherStorage(t), nil)
	require.NoError(t, e)
	_, e = publisher.Publish(ctx, "qa-other", machinePublisherStorage(t), nil)
	require.Error(t, e, "the token cannot publish another image")
}
