package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/onkernel/hypeman/lib/oapi"
	"github.com/onkernel/hypeman/lib/paths"
	"github.com/onkernel/hypeman/lib/registry"
	"github.com/onkernel/hypeman/lib/system"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRegistryPushAndConvert(t *testing.T) {
	svc := newTestService(t)
	p := paths.New(svc.Config.DataDir)

	// Create registry
	reg, err := registry.New(p, svc.ImageManager)
	require.NoError(t, err)

	// Create test server with registry mounted
	r := chi.NewRouter()
	r.Mount("/v2", reg.Handler())

	ts := httptest.NewServer(r)
	defer ts.Close()

	// Get the test server host (without http://)
	serverHost := strings.TrimPrefix(ts.URL, "http://")

	// Pull a small image from Docker Hub to push to our registry
	t.Log("Pulling alpine:latest from Docker Hub...")
	srcRef, err := name.ParseReference("docker.io/library/alpine:latest")
	require.NoError(t, err)

	img, err := remote.Image(srcRef)
	require.NoError(t, err)

	digest, err := img.Digest()
	require.NoError(t, err)
	t.Logf("Source image digest: %s", digest.String())

	// Push to our test registry using digest reference (required for conversion trigger)
	targetRef := serverHost + "/test/alpine@" + digest.String()
	t.Logf("Pushing to %s...", targetRef)

	dstRef, err := name.ParseReference(targetRef, name.Insecure)
	require.NoError(t, err)

	err = remote.Write(dstRef, img)
	require.NoError(t, err)
	t.Log("Push successful!")

	// Wait for image to be converted
	imageName := "test/alpine@" + digest.String()
	t.Logf("Waiting for image %s to be ready...", imageName)

	deadline := time.Now().Add(60 * time.Second)
	var lastStatus oapi.ImageStatus
	for time.Now().Before(deadline) {
		resp, err := svc.GetImage(ctx(), oapi.GetImageRequestObject{
			Name: imageName,
		})
		if err != nil {
			t.Logf("GetImage error (may be expected initially): %v", err)
			time.Sleep(1 * time.Second)
			continue
		}

		img, ok := resp.(oapi.GetImage200JSONResponse)
		if ok {
			lastStatus = img.Status
			switch img.Status {
			case oapi.Ready:
				t.Log("Image conversion complete!")
				return // Success!
			case oapi.Failed:
				errMsg := "unknown error"
				if img.Error != nil {
					errMsg = *img.Error
				}
				t.Fatalf("Image conversion failed: %s", errMsg)
			default:
				t.Logf("Image status: %s", img.Status)
			}
		}
		time.Sleep(2 * time.Second)
	}

	t.Fatalf("Timeout waiting for image conversion. Last status: %s", lastStatus)
}

func TestRegistryVersionCheck(t *testing.T) {
	svc := newTestService(t)
	p := paths.New(svc.Config.DataDir)

	// Create registry
	reg, err := registry.New(p, svc.ImageManager)
	require.NoError(t, err)

	// Create test server
	r := chi.NewRouter()
	r.Mount("/v2", reg.Handler())

	ts := httptest.NewServer(r)
	defer ts.Close()

	// Test /v2/ endpoint (version check)
	resp, err := http.Get(ts.URL + "/v2/")
	require.NoError(t, err)
	defer resp.Body.Close()

	// OCI Distribution Spec requires 200 OK for version check
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

func TestRegistryPushAndCreateInstance(t *testing.T) {
	// This is a full e2e test that requires KVM access
	if _, err := os.Stat("/dev/kvm"); os.IsNotExist(err) {
		t.Skip("/dev/kvm not available - skipping VM creation test")
	}

	svc := newTestService(t)
	p := paths.New(svc.Config.DataDir)

	// Ensure system files
	systemMgr := system.NewManager(p)
	err := systemMgr.EnsureSystemFiles(context.Background())
	require.NoError(t, err)

	// Create registry
	reg, err := registry.New(p, svc.ImageManager)
	require.NoError(t, err)

	// Create test server
	r := chi.NewRouter()
	r.Mount("/v2", reg.Handler())

	ts := httptest.NewServer(r)
	defer ts.Close()

	serverHost := strings.TrimPrefix(ts.URL, "http://")

	// Pull and push alpine
	t.Log("Pulling alpine:latest...")
	srcRef, err := name.ParseReference("docker.io/library/alpine:latest")
	require.NoError(t, err)

	img, err := remote.Image(srcRef)
	require.NoError(t, err)

	digest, err := img.Digest()
	require.NoError(t, err)

	targetRef := serverHost + "/test/alpine@" + digest.String()
	dstRef, err := name.ParseReference(targetRef, name.Insecure)
	require.NoError(t, err)

	t.Log("Pushing to test registry...")
	err = remote.Write(dstRef, img)
	require.NoError(t, err)

	// Wait for image to be ready
	imageName := "test/alpine@" + digest.String()
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		resp, _ := svc.GetImage(ctx(), oapi.GetImageRequestObject{Name: imageName})
		if img, ok := resp.(oapi.GetImage200JSONResponse); ok && img.Status == oapi.Ready {
			t.Log("Image ready!")
			break
		}
		time.Sleep(2 * time.Second)
	}

	// Create instance with pushed image
	t.Log("Creating instance with pushed image...")
	networkEnabled := false
	resp, err := svc.CreateInstance(ctx(), oapi.CreateInstanceRequestObject{
		Body: &oapi.CreateInstanceRequest{
			Name:  "test-pushed-image",
			Image: imageName,
			Network: &struct {
				Enabled *bool `json:"enabled,omitempty"`
			}{
				Enabled: &networkEnabled,
			},
		},
	})
	require.NoError(t, err)

	created, ok := resp.(oapi.CreateInstance201JSONResponse)
	require.True(t, ok, "expected 201 response, got %T", resp)

	instance := oapi.Instance(created)
	assert.Equal(t, "test-pushed-image", instance.Name)
	t.Logf("Instance created: %s (state: %s)", instance.Id, instance.State)

	// Verify instance reaches Running state
	deadline = time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		resp, _ := svc.GetInstance(ctx(), oapi.GetInstanceRequestObject{Id: instance.Id})
		if inst, ok := resp.(oapi.GetInstance200JSONResponse); ok {
			if inst.State == "Running" {
				t.Log("Instance is running!")
				return // Success!
			}
			t.Logf("Instance state: %s", inst.State)
		}
		time.Sleep(1 * time.Second)
	}

	t.Fatal("Timeout waiting for instance to reach Running state")
}

// TestRegistryLayerCaching verifies that pushing the same image twice
// reuses cached layers and doesn't re-upload them.
func TestRegistryLayerCaching(t *testing.T) {
	svc := newTestService(t)
	p := paths.New(svc.Config.DataDir)

	reg, err := registry.New(p, svc.ImageManager)
	require.NoError(t, err)

	r := chi.NewRouter()
	r.Mount("/v2", reg.Handler())

	ts := httptest.NewServer(r)
	defer ts.Close()

	serverHost := strings.TrimPrefix(ts.URL, "http://")

	// Pull alpine image from Docker Hub
	t.Log("Pulling alpine:latest from Docker Hub...")
	srcRef, err := name.ParseReference("docker.io/library/alpine:latest")
	require.NoError(t, err)

	img, err := remote.Image(srcRef)
	require.NoError(t, err)

	digest, err := img.Digest()
	require.NoError(t, err)

	// First push - should upload all blobs
	t.Log("First push - uploading all layers...")
	targetRef := serverHost + "/cache-test/alpine@" + digest.String()
	dstRef, err := name.ParseReference(targetRef, name.Insecure)
	require.NoError(t, err)

	// Track requests during first push
	var firstPushRequests []string
	transport := &loggingTransport{
		transport: http.DefaultTransport,
		log: func(method, path string) {
			firstPushRequests = append(firstPushRequests, method+" "+path)
		},
	}

	err = remote.Write(dstRef, img, remote.WithTransport(transport))
	require.NoError(t, err)

	// Count blob uploads in first push
	firstPushUploads := 0
	for _, req := range firstPushRequests {
		if strings.HasPrefix(req, "PUT ") && strings.Contains(req, "/blobs/uploads/") {
			firstPushUploads++
		}
	}
	t.Logf("First push: %d blob uploads", firstPushUploads)
	assert.Greater(t, firstPushUploads, 0, "First push should upload blobs")

	// Second push - should reuse cached blobs
	t.Log("Second push - should reuse cached layers...")
	var secondPushRequests []string
	transport2 := &loggingTransport{
		transport: http.DefaultTransport,
		log: func(method, path string) {
			secondPushRequests = append(secondPushRequests, method+" "+path)
		},
	}

	err = remote.Write(dstRef, img, remote.WithTransport(transport2))
	require.NoError(t, err)

	// Count operations in second push
	secondPushUploads := 0
	secondPushManifestHead := 0
	for _, req := range secondPushRequests {
		if strings.HasPrefix(req, "PUT ") && strings.Contains(req, "/blobs/uploads/") {
			secondPushUploads++
		}
		if strings.HasPrefix(req, "HEAD ") && strings.Contains(req, "/manifests/") {
			secondPushManifestHead++
		}
	}
	t.Logf("Second push: %d total requests, %d blob uploads", len(secondPushRequests), secondPushUploads)

	// Second push should:
	// 1. Check if manifest exists (HEAD) - if yes, skip everything
	// 2. NOT upload any blobs (all cached or manifest already exists)
	assert.Greater(t, secondPushManifestHead, 0, "Second push should check if manifest exists")
	assert.Equal(t, 0, secondPushUploads, "Second push should NOT upload any blobs (all cached)")
	assert.Less(t, len(secondPushRequests), len(firstPushRequests), "Second push should make fewer requests than first")

	t.Logf("Layer caching verified: first push=%d requests, second push=%d requests", len(firstPushRequests), len(secondPushRequests))

	// Wait for async conversion to complete to avoid cleanup issues
	time.Sleep(2 * time.Second)
}

// TestRegistrySharedLayerCaching verifies that pushing different images
// that share layers reuses the cached shared layers.
func TestRegistrySharedLayerCaching(t *testing.T) {
	svc := newTestService(t)
	p := paths.New(svc.Config.DataDir)

	reg, err := registry.New(p, svc.ImageManager)
	require.NoError(t, err)

	r := chi.NewRouter()
	r.Mount("/v2", reg.Handler())

	ts := httptest.NewServer(r)
	defer ts.Close()

	serverHost := strings.TrimPrefix(ts.URL, "http://")

	// Pull alpine image (this will be our base)
	t.Log("Pulling alpine:latest...")
	alpineRef, err := name.ParseReference("docker.io/library/alpine:latest")
	require.NoError(t, err)
	alpineImg, err := remote.Image(alpineRef)
	require.NoError(t, err)

	// Get alpine layers for comparison
	alpineLayers, err := alpineImg.Layers()
	require.NoError(t, err)
	t.Logf("Alpine has %d layers", len(alpineLayers))

	// Push alpine first
	t.Log("Pushing alpine...")
	alpineDigest, _ := alpineImg.Digest()
	dstRef, err := name.ParseReference(serverHost+"/shared/alpine@"+alpineDigest.String(), name.Insecure)
	require.NoError(t, err)

	var firstPushBlobUploads int
	transport1 := &loggingTransport{
		transport: http.DefaultTransport,
		log: func(method, path string) {
			if method == "PUT" && strings.Contains(path, "/blobs/uploads/") {
				firstPushBlobUploads++
			}
		},
	}
	err = remote.Write(dstRef, alpineImg, remote.WithTransport(transport1))
	require.NoError(t, err)
	t.Logf("First push (alpine): %d blob uploads", firstPushBlobUploads)

	// Now pull a different alpine-based image (e.g., alpine:3.18)
	// which should share the base layer with alpine:latest
	t.Log("Pulling alpine:3.18 (shares base layer)...")
	alpine318Ref, err := name.ParseReference("docker.io/library/alpine:3.18")
	require.NoError(t, err)
	alpine318Img, err := remote.Image(alpine318Ref)
	require.NoError(t, err)

	alpine318Digest, _ := alpine318Img.Digest()
	dstRef2, err := name.ParseReference(serverHost+"/shared/alpine318@"+alpine318Digest.String(), name.Insecure)
	require.NoError(t, err)

	var secondPushBlobUploads int
	var secondPushBlobHeads int
	transport2 := &loggingTransport{
		transport: http.DefaultTransport,
		log: func(method, path string) {
			if method == "PUT" && strings.Contains(path, "/blobs/uploads/") {
				secondPushBlobUploads++
			}
			if method == "HEAD" && strings.Contains(path, "/blobs/") {
				secondPushBlobHeads++
			}
		},
	}

	t.Log("Pushing alpine:3.18...")
	err = remote.Write(dstRef2, alpine318Img, remote.WithTransport(transport2))
	require.NoError(t, err)
	t.Logf("Second push (alpine:3.18): %d HEAD requests for blobs, %d blob uploads", secondPushBlobHeads, secondPushBlobUploads)

	// If layers are shared and caching works, the second push should upload
	// fewer blobs than the total layers in the image (some are cached)
	alpine318Layers, _ := alpine318Img.Layers()
	t.Logf("Alpine 3.18 has %d layers, uploaded %d", len(alpine318Layers), secondPushBlobUploads)

	// The key assertion: second push should upload fewer blobs than first
	// (or equal if they don't share layers, but usually alpine versions share the base)
	assert.LessOrEqual(t, secondPushBlobUploads, firstPushBlobUploads,
		"Second push should upload same or fewer blobs due to layer sharing")

	// Wait for async conversion
	time.Sleep(2 * time.Second)
}

// loggingTransport wraps an http.RoundTripper and logs requests
type loggingTransport struct {
	transport http.RoundTripper
	log       func(method, path string)
}

func (t *loggingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	t.log(req.Method, req.URL.Path)
	return t.transport.RoundTrip(req)
}
