package builds_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

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

func machinePublisherBundle(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	platform := images.MacOSImage{HardwareModel: []byte{1}, MachineIdentifier: []byte{2}, MAC: "02:00:00:00:00:01", CPUs: 4, Memory: 8 << 30, GuestAgent: true}
	data, e := json.Marshal(platform)
	require.NoError(t, e)
	for name, data := range map[string][]byte{"config.json": data, "disk.img": []byte("synthetic disk"), "aux.img": []byte("synthetic aux")} {
		require.NoError(t, os.WriteFile(filepath.Join(root, name), data, 0600))
	}
	return root
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
	result, e := publisher.Publish(ctx, "qa-build", machinePublisherBundle(t))
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
	_, e := publisher.Publish(ctx, "qa-build", machinePublisherBundle(t))
	require.Error(t, e)
	require.False(t, visited.Load())
}
