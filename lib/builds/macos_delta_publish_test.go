package builds_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/types"
	"github.com/kernel/hypeman/lib/builds"
	"github.com/kernel/hypeman/lib/images"
	"github.com/kernel/hypeman/lib/middleware"
	"github.com/kernel/hypeman/lib/paths"
	"github.com/kernel/hypeman/lib/registry"
	"github.com/stretchr/testify/require"
)

const deltaTestSecret = "synthetic registry secret"

type deltaTestRegistry struct {
	server  *httptest.Server
	tokens  *builds.RegistryTokenGenerator
	mounts  atomic.Int32
	uploads atomic.Int32
	puts    atomic.Int32
}

func newDeltaTestRegistry(t *testing.T) *deltaTestRegistry {
	t.Helper()
	imports := &machineImportStub{imported: make(chan string, 16)}
	reg, err := registry.New(paths.New(t.TempDir()), imports)
	require.NoError(t, err)
	dr := &deltaTestRegistry{tokens: builds.NewRegistryTokenGenerator(deltaTestSecret)}
	handler := middleware.JwtAuth(deltaTestSecret)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Query().Get("mount") != "" {
			dr.mounts.Add(1)
		} else if r.Method == http.MethodPost && r.URL.Query().Get("digest") != "" {
			dr.uploads.Add(1)
		}
		if r.Method == http.MethodPut && strings.Contains(r.URL.Path, "/manifests/") {
			dr.puts.Add(1)
		}
		reg.Handler().ServeHTTP(w, r)
	}))
	dr.server = httptest.NewServer(handler)
	t.Cleanup(dr.server.Close)
	// Each manifest PUT starts a conversion goroutine that writes the OCI layout
	// after the response. Its import call is the last step, so wait for every one
	// before the TempDir cleanup removes the registry's data.
	t.Cleanup(func() {
		for range dr.puts.Load() {
			select {
			case <-imports.imported:
			case <-time.After(10 * time.Second):
				return
			}
		}
	})
	return dr
}

// deltaTestDisk is a disk of eight 64 KiB chunks, each a distinct byte.
func deltaTestDisk() []byte {
	disk := make([]byte, 8*64<<10)
	for i := range 8 {
		for j := range 64 << 10 {
			disk[i*64<<10+j] = byte(i + 1)
		}
	}
	return disk
}

func deltaTestBundle(t *testing.T, disk []byte, aux string) string {
	t.Helper()
	bundle := machinePublisherBundle(t)
	require.NoError(t, os.WriteFile(filepath.Join(bundle, "disk.img"), disk, 0600))
	require.NoError(t, os.WriteFile(filepath.Join(bundle, "aux.img"), []byte(aux), 0600))
	return bundle
}

// pullManifest fetches a manifest from the registry with a pull-scoped token.
func (dr *deltaTestRegistry) pullManifest(t *testing.T, repo, digest string) v1.Manifest {
	t.Helper()
	token, err := dr.tokens.GenerateToken("reader", []builds.RepoPermission{{Repo: repo, Scope: "pull"}}, time.Minute)
	require.NoError(t, err)
	request, err := http.NewRequest(http.MethodGet, dr.server.URL+"/v2/"+repo+"/manifests/"+digest, nil)
	require.NoError(t, err)
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Accept", string(types.OCIManifestSchema1))
	response, err := http.DefaultClient.Do(request)
	require.NoError(t, err)
	defer response.Body.Close()
	require.Equal(t, http.StatusOK, response.StatusCode)
	var manifest v1.Manifest
	require.NoError(t, json.NewDecoder(response.Body).Decode(&manifest))
	return manifest
}

func TestStreamingMachinePublisherDerivedMountsBaseLayers(t *testing.T) {
	dr := newDeltaTestRegistry(t)
	publisher := &builds.StreamingMachinePublisher{RegistryURL: dr.server.URL, Tokens: dr.tokens}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	baseBundle := deltaTestBundle(t, deltaTestDisk(), "base aux")
	base, err := publisher.Publish(ctx, "base-x", baseBundle)
	require.NoError(t, err)
	baseManifest := dr.pullManifest(t, "builds/base-x", base.Digest)

	derived := deltaTestDisk()
	for j := range 64 << 10 {
		derived[3*64<<10+j] = 0xAA
	}
	derivedBundle := deltaTestBundle(t, derived, "derived aux")
	dr.mounts.Store(0)
	dr.uploads.Store(0)
	result, err := publisher.PublishDerived(ctx, "derived-x", derivedBundle, builds.MachineDeltaBase{
		Repo:     "builds/base-x",
		Digest:   base.Digest,
		DiskPath: filepath.Join(baseBundle, "disk.img"),
	})
	require.NoError(t, err)
	require.Equal(t, int32(1), dr.mounts.Load(), "the base layer is mounted, not uploaded")
	require.Equal(t, int32(2), dr.uploads.Load(), "only the config and the delta layer are uploaded")

	derivedManifest := dr.pullManifest(t, "builds/derived-x", result.Digest)
	require.Len(t, derivedManifest.Layers, 2)
	require.Equal(t, baseManifest.Layers[0].Digest, derivedManifest.Layers[0].Digest)
	require.Equal(t, images.MacOSDeltaMediaType, derivedManifest.Layers[1].MediaType)
}

func TestStreamingMachinePublisherDerivedRejectsMissingBase(t *testing.T) {
	dr := newDeltaTestRegistry(t)
	publisher := &builds.StreamingMachinePublisher{RegistryURL: dr.server.URL, Tokens: dr.tokens}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	bundle := deltaTestBundle(t, deltaTestDisk(), "aux")
	missing := fmt.Sprintf("sha256:%064x", 1)
	_, err := publisher.PublishDerived(ctx, "derived-y", bundle, builds.MachineDeltaBase{
		Repo:     "builds/never-published",
		Digest:   missing,
		DiskPath: filepath.Join(bundle, "disk.img"),
	})
	require.Error(t, err)
}

func TestStreamingMachinePublisherDerivedRejectsNonBaseRepo(t *testing.T) {
	dr := newDeltaTestRegistry(t)
	publisher := &builds.StreamingMachinePublisher{RegistryURL: dr.server.URL, Tokens: dr.tokens}
	bundle := deltaTestBundle(t, deltaTestDisk(), "aux")
	_, err := publisher.PublishDerived(context.Background(), "derived-z", bundle, builds.MachineDeltaBase{
		Repo:     "library/alpine",
		Digest:   fmt.Sprintf("sha256:%064x", 2),
		DiskPath: filepath.Join(bundle, "disk.img"),
	})
	require.ErrorContains(t, err, "under builds/")
}

// mountRequest posts a blob mount with the given token and reports the status.
func (dr *deltaTestRegistry) mountRequest(t *testing.T, token, repo, digest, from string) int {
	t.Helper()
	target := dr.server.URL + "/v2/" + repo + "/blobs/uploads/?mount=" + digest + "&from=" + from
	request, err := http.NewRequest(http.MethodPost, target, bytes.NewReader(nil))
	require.NoError(t, err)
	request.Header.Set("Authorization", "Bearer "+token)
	response, err := http.DefaultClient.Do(request)
	require.NoError(t, err)
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, response.Body)
	return response.StatusCode
}

func TestBlobMountFallsThroughToUploadForMissingBlob(t *testing.T) {
	dr := newDeltaTestRegistry(t)
	token, err := dr.tokens.GenerateToken("mounter", []builds.RepoPermission{
		{Repo: "builds/target-x", Scope: "push"},
		{Repo: "builds/source-x", Scope: "pull"},
	}, time.Minute)
	require.NoError(t, err)
	status := dr.mountRequest(t, token, "builds/target-x", fmt.Sprintf("sha256:%064x", 3), "builds/source-x")
	require.Equal(t, http.StatusAccepted, status, "a missing blob opens an upload session")
}

func TestBlobMountRefusedFromUnauthorizedSource(t *testing.T) {
	dr := newDeltaTestRegistry(t)
	token, err := dr.tokens.GenerateToken("mounter", []builds.RepoPermission{
		{Repo: "builds/target-x", Scope: "push"},
	}, time.Minute)
	require.NoError(t, err)
	status := dr.mountRequest(t, token, "builds/target-x", fmt.Sprintf("sha256:%064x", 4), "builds/other-private")
	require.Equal(t, http.StatusForbidden, status, "a token without pull on the source cannot mount from it")
}
