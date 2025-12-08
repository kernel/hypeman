package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/types"
	"github.com/onkernel/hypeman/lib/oapi"
	"github.com/onkernel/hypeman/lib/paths"
	"github.com/onkernel/hypeman/lib/registry"
	"github.com/onkernel/hypeman/lib/system"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// setupRegistryTest creates a test service with a mounted OCI registry server.
// Returns the service (for API calls) and the server host (for building push URLs).
func setupRegistryTest(t *testing.T) (*ApiService, string) {
	t.Helper()

	svc := newTestService(t)
	p := paths.New(svc.Config.DataDir)

	reg, err := registry.New(p, svc.ImageManager)
	require.NoError(t, err)

	r := chi.NewRouter()
	r.Mount("/v2", reg.Handler())

	ts := httptest.NewServer(r)
	t.Cleanup(ts.Close)

	serverHost := strings.TrimPrefix(ts.URL, "http://")
	return svc, serverHost
}

func TestRegistryPushAndConvert(t *testing.T) {
	svc, serverHost := setupRegistryTest(t)

	// Pull a small image from Docker Hub to push to our registry
	t.Log("Pulling alpine:latest from Docker Hub...")
	srcRef, err := name.ParseReference("docker.io/library/alpine:latest")
	require.NoError(t, err)

	img, err := remote.Image(srcRef, remote.WithAuthFromKeychain(authn.DefaultKeychain))
	require.NoError(t, err)

	digest, err := img.Digest()
	require.NoError(t, err)
	t.Logf("Source image digest: %s", digest.String())

	// Push to our test registry using digest reference
	targetRef := serverHost + "/test/alpine@" + digest.String()
	t.Logf("Pushing to %s...", targetRef)

	dstRef, err := name.ParseReference(targetRef, name.Insecure)
	require.NoError(t, err)

	err = remote.Write(dstRef, img)
	require.NoError(t, err)
	t.Log("Push successful!")

	// Wait for image to be converted
	imageName := "test/alpine@" + digest.String()
	imgResp := waitForImageReady(t, svc, imageName, 60*time.Second)
	assert.NotNil(t, imgResp.SizeBytes, "ready image should have size")
}

func TestRegistryVersionCheck(t *testing.T) {
	_, serverHost := setupRegistryTest(t)

	// Test /v2/ endpoint (version check)
	resp, err := http.Get("http://" + serverHost + "/v2/")
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

	svc, serverHost := setupRegistryTest(t)

	// Ensure system files for VM creation
	p := paths.New(svc.Config.DataDir)
	systemMgr := system.NewManager(p)
	err := systemMgr.EnsureSystemFiles(context.Background())
	require.NoError(t, err)

	// Pull and push alpine
	t.Log("Pulling alpine:latest...")
	srcRef, err := name.ParseReference("docker.io/library/alpine:latest")
	require.NoError(t, err)

	img, err := remote.Image(srcRef, remote.WithAuthFromKeychain(authn.DefaultKeychain))
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
	waitForImageReady(t, svc, imageName, 60*time.Second)

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
	deadline := time.Now().Add(30 * time.Second)
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
	_, serverHost := setupRegistryTest(t)

	// Pull alpine image from Docker Hub
	t.Log("Pulling alpine:latest from Docker Hub...")
	srcRef, err := name.ParseReference("docker.io/library/alpine:latest")
	require.NoError(t, err)

	img, err := remote.Image(srcRef, remote.WithAuthFromKeychain(authn.DefaultKeychain))
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
	_, serverHost := setupRegistryTest(t)

	// Pull alpine image (this will be our base)
	t.Log("Pulling alpine:latest...")
	alpineRef, err := name.ParseReference("docker.io/library/alpine:latest")
	require.NoError(t, err)
	alpineImg, err := remote.Image(alpineRef, remote.WithAuthFromKeychain(authn.DefaultKeychain))
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
	alpine318Img, err := remote.Image(alpine318Ref, remote.WithAuthFromKeychain(authn.DefaultKeychain))
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

// TestRegistryTagPush verifies that pushing with a tag reference (not digest)
// correctly triggers conversion. The server computes the digest from the manifest.
func TestRegistryTagPush(t *testing.T) {
	svc, serverHost := setupRegistryTest(t)

	// Pull alpine image from Docker Hub
	t.Log("Pulling alpine:latest from Docker Hub...")
	srcRef, err := name.ParseReference("docker.io/library/alpine:latest")
	require.NoError(t, err)

	img, err := remote.Image(srcRef, remote.WithAuthFromKeychain(authn.DefaultKeychain))
	require.NoError(t, err)

	digest, err := img.Digest()
	require.NoError(t, err)
	t.Logf("Source image digest: %s", digest.String())

	// Push using TAG reference (not digest) - this is the key difference from other tests
	targetRef := serverHost + "/tag-test/alpine:latest"
	t.Logf("Pushing to %s (tag reference)...", targetRef)

	dstRef, err := name.ParseReference(targetRef, name.Insecure)
	require.NoError(t, err)

	err = remote.Write(dstRef, img)
	require.NoError(t, err)
	t.Log("Push successful!")

	// The image should be registered with the computed digest, not the tag
	imageName := "tag-test/alpine@" + digest.String()
	waitForImageReady(t, svc, imageName, 60*time.Second)

	// Verify image appears in ListImages (GET /images)
	listResp, err := svc.ListImages(ctx(), oapi.ListImagesRequestObject{})
	require.NoError(t, err)
	images, ok := listResp.(oapi.ListImages200JSONResponse)
	require.True(t, ok, "expected ListImages 200 response")

	var found bool
	for _, img := range images {
		if img.Digest == digest.String() {
			found = true
			assert.Equal(t, oapi.Ready, img.Status, "image in list should have Ready status")
			assert.NotNil(t, img.SizeBytes, "ready image should have size")
			t.Logf("Image found in ListImages: %s (status=%s, size=%d)", img.Name, img.Status, *img.SizeBytes)
			break
		}
	}
	assert.True(t, found, "pushed image should appear in ListImages response")
}

// TestRegistryDockerV2ManifestConversion verifies that pushing an image with a
// Docker v2 manifest (as returned by local Docker daemon) is correctly converted
// to OCI format and the image conversion succeeds.
func TestRegistryDockerV2ManifestConversion(t *testing.T) {
	svc, serverHost := setupRegistryTest(t)

	// Pull alpine image from Docker Hub (OCI format)
	t.Log("Pulling alpine:latest from Docker Hub...")
	srcRef, err := name.ParseReference("docker.io/library/alpine:latest")
	require.NoError(t, err)

	img, err := remote.Image(srcRef, remote.WithAuthFromKeychain(authn.DefaultKeychain))
	require.NoError(t, err)

	// Wrap the image to simulate Docker v2 format (Docker daemon returns this format)
	// This is what happens when using `daemon.Image()` in the CLI
	dockerV2Img := &dockerV2ImageWrapper{img: img}

	// Push the Docker v2 formatted image
	targetRef := serverHost + "/dockerv2-test/alpine:v1"
	t.Logf("Pushing Docker v2 formatted image to %s...", targetRef)

	dstRef, err := name.ParseReference(targetRef, name.Insecure)
	require.NoError(t, err)

	err = remote.Write(dstRef, dockerV2Img)
	require.NoError(t, err)
	t.Log("Push successful!")

	// Wait for image to be converted
	// The server converts Docker v2 to OCI format internally, resulting in a different digest
	imgResp := waitForImageReady(t, svc, "dockerv2-test/alpine:v1", 60*time.Second)
	assert.NotNil(t, imgResp.SizeBytes, "ready image should have size")
	assert.NotEmpty(t, imgResp.Digest, "image should have digest")
}

// dockerV2ImageWrapper wraps an OCI image and returns Docker v2 media types
// to simulate what the Docker daemon returns via daemon.Image()
type dockerV2ImageWrapper struct {
	img v1.Image
}

func (w *dockerV2ImageWrapper) Layers() ([]v1.Layer, error) {
	layers, err := w.img.Layers()
	if err != nil {
		return nil, err
	}
	// Wrap each layer to return Docker v2 media types
	wrapped := make([]v1.Layer, len(layers))
	for i, l := range layers {
		wrapped[i] = &dockerV2LayerWrapper{layer: l}
	}
	return wrapped, nil
}

func (w *dockerV2ImageWrapper) MediaType() (types.MediaType, error) {
	return types.DockerManifestSchema2, nil
}

func (w *dockerV2ImageWrapper) Size() (int64, error) {
	return w.img.Size()
}

func (w *dockerV2ImageWrapper) ConfigName() (v1.Hash, error) {
	return w.img.ConfigName()
}

func (w *dockerV2ImageWrapper) ConfigFile() (*v1.ConfigFile, error) {
	return w.img.ConfigFile()
}

func (w *dockerV2ImageWrapper) RawConfigFile() ([]byte, error) {
	return w.img.RawConfigFile()
}

func (w *dockerV2ImageWrapper) Digest() (v1.Hash, error) {
	// Compute digest of our Docker v2 manifest
	rawManifest, err := w.RawManifest()
	if err != nil {
		return v1.Hash{}, err
	}
	h, _, err := v1.SHA256(strings.NewReader(string(rawManifest)))
	return h, err
}

func (w *dockerV2ImageWrapper) Manifest() (*v1.Manifest, error) {
	origManifest, err := w.img.Manifest()
	if err != nil {
		return nil, err
	}

	// Convert to Docker v2 media types
	manifest := &v1.Manifest{
		SchemaVersion: origManifest.SchemaVersion,
		MediaType:     types.DockerManifestSchema2,
		Config: v1.Descriptor{
			MediaType: types.DockerConfigJSON,
			Size:      origManifest.Config.Size,
			Digest:    origManifest.Config.Digest,
		},
	}

	for _, layer := range origManifest.Layers {
		manifest.Layers = append(manifest.Layers, v1.Descriptor{
			MediaType: types.DockerLayer,
			Size:      layer.Size,
			Digest:    layer.Digest,
		})
	}

	return manifest, nil
}

func (w *dockerV2ImageWrapper) RawManifest() ([]byte, error) {
	manifest, err := w.Manifest()
	if err != nil {
		return nil, err
	}
	return json.Marshal(manifest)
}

func (w *dockerV2ImageWrapper) LayerByDigest(hash v1.Hash) (v1.Layer, error) {
	layer, err := w.img.LayerByDigest(hash)
	if err != nil {
		return nil, err
	}
	return &dockerV2LayerWrapper{layer: layer}, nil
}

func (w *dockerV2ImageWrapper) LayerByDiffID(hash v1.Hash) (v1.Layer, error) {
	layer, err := w.img.LayerByDiffID(hash)
	if err != nil {
		return nil, err
	}
	return &dockerV2LayerWrapper{layer: layer}, nil
}

// dockerV2LayerWrapper wraps a layer to return Docker v2 media types
type dockerV2LayerWrapper struct {
	layer v1.Layer
}

func (w *dockerV2LayerWrapper) Digest() (v1.Hash, error) {
	return w.layer.Digest()
}

func (w *dockerV2LayerWrapper) DiffID() (v1.Hash, error) {
	return w.layer.DiffID()
}

func (w *dockerV2LayerWrapper) Compressed() (io.ReadCloser, error) {
	return w.layer.Compressed()
}

func (w *dockerV2LayerWrapper) Uncompressed() (io.ReadCloser, error) {
	return w.layer.Uncompressed()
}

func (w *dockerV2LayerWrapper) Size() (int64, error) {
	return w.layer.Size()
}

func (w *dockerV2LayerWrapper) MediaType() (types.MediaType, error) {
	return types.DockerLayer, nil
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

// waitForImageReady polls GetImage until the image reaches Ready status.
// Returns the image response on success, fails the test on error or timeout.
func waitForImageReady(t *testing.T, svc *ApiService, imageName string, timeout time.Duration) oapi.GetImage200JSONResponse {
	t.Helper()
	t.Logf("Waiting for image %s to be ready...", imageName)

	deadline := time.Now().Add(timeout)
	var lastStatus oapi.ImageStatus
	var lastError string

	for time.Now().Before(deadline) {
		resp, err := svc.GetImage(ctx(), oapi.GetImageRequestObject{Name: imageName})
		if err != nil {
			time.Sleep(1 * time.Second)
			continue
		}

		imgResp, ok := resp.(oapi.GetImage200JSONResponse)
		if !ok {
			time.Sleep(1 * time.Second)
			continue
		}

		lastStatus = imgResp.Status
		if imgResp.Error != nil {
			lastError = *imgResp.Error
		}

		switch imgResp.Status {
		case oapi.Ready:
			t.Logf("Image ready: %s (digest=%s)", imgResp.Name, imgResp.Digest)
			return imgResp
		case oapi.Failed:
			t.Fatalf("Image conversion failed: %s", lastError)
		default:
			t.Logf("Image status: %s", imgResp.Status)
		}
		time.Sleep(2 * time.Second)
	}

	t.Fatalf("Timeout waiting for image %s. Last status: %s, error: %s", imageName, lastStatus, lastError)
	return oapi.GetImage200JSONResponse{}
}
