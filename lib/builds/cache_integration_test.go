package builds

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/network"
	"github.com/testcontainers/testcontainers-go/wait"
)

// TestBuildKitCacheContainsLayerBlobs verifies that when BuildKit exports a cache
// with image-manifest=true, the cache image contains actual layer blobs (not just
// references to external registries like Docker Hub).
//
// This test reproduces the issue where:
// 1. Admin build populates global cache with image-manifest=true
// 2. New tenant's first build imports this cache
// 3. Despite the import, FROM instruction still downloads base image layers from Docker Hub
//
// The root cause is that BuildKit's registry cache stores metadata for FROM instructions,
// but the actual base image layers are referenced (not copied) to the cache.
func TestBuildKitCacheContainsLayerBlobs(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	ctx := context.Background()

	// Create a shared network for the containers
	testNetwork, err := network.New(ctx)
	require.NoError(t, err, "failed to create network")
	defer testNetwork.Remove(ctx)

	networkName := testNetwork.Name

	// Start registry container
	registryC, registryHostExternal, registryHostInternal := startRegistryContainer(t, ctx, networkName)
	defer registryC.Terminate(ctx)

	// Start BuildKit container on the same network
	buildkitC := startBuildKitContainer(t, ctx, networkName)
	defer buildkitC.Terminate(ctx)

	// Use internal hostname for BuildKit to reach registry
	registryHost := registryHostInternal
	// Use external hostname for test to reach registry
	buildkitHost := registryHostExternal

	// Create test Dockerfile that uses a base image
	srcDir := t.TempDir()
	dockerfile := `FROM alpine:3.21
RUN echo "test layer 1" > /layer1.txt
RUN echo "test layer 2" > /layer2.txt
`
	require.NoError(t, os.WriteFile(filepath.Join(srcDir, "Dockerfile"), []byte(dockerfile), 0644))

	// Copy source to BuildKit container
	copyToContainer(t, ctx, buildkitC, srcDir, "/src")

	// Build and export cache with image-manifest=true
	cacheRef := fmt.Sprintf("%s/cache/test/alpine-cache:v1", registryHost)
	outputRef := fmt.Sprintf("%s/builds/test-build:v1", registryHost)

	t.Log("Building image and exporting cache with image-manifest=true...")
	buildCmd := []string{
		"buildctl", "build",
		"--frontend", "dockerfile.v0",
		"--local", "context=/src",
		"--local", "dockerfile=/src",
		"--output", fmt.Sprintf("type=image,name=%s,push=true,registry.insecure=true", outputRef),
		"--export-cache", fmt.Sprintf("type=registry,ref=%s,mode=max,image-manifest=true,oci-mediatypes=true,registry.insecure=true", cacheRef),
	}

	exitCode, output, err := execInContainer(t, ctx, buildkitC, buildCmd)
	t.Logf("BuildKit output:\n%s", output)
	require.NoError(t, err)
	require.Equal(t, 0, exitCode, "buildctl failed with exit code %d", exitCode)

	// Now inspect the cache image to verify it contains layer blobs
	t.Log("Inspecting cache image...")

	// Fetch the cache manifest from registry
	manifestURL := fmt.Sprintf("http://%s/v2/cache/test/alpine-cache/manifests/v1", buildkitHost)
	req, _ := http.NewRequest("GET", manifestURL, nil)
	req.Header.Set("Accept", "application/vnd.oci.image.manifest.v1+json, application/vnd.docker.distribution.manifest.v2+json, application/vnd.oci.image.index.v1+json")

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err, "failed to fetch cache manifest")
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	t.Logf("Cache manifest response (status %d):\n%s", resp.StatusCode, string(body))

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("Failed to fetch cache manifest: status %d", resp.StatusCode)
	}

	var manifest struct {
		MediaType string `json:"mediaType"`
		Layers    []struct {
			MediaType string   `json:"mediaType"`
			Size      int64    `json:"size"`
			Digest    string   `json:"digest"`
			URLs      []string `json:"urls,omitempty"` // Foreign layer URLs
		} `json:"layers"`
	}
	require.NoError(t, json.Unmarshal(body, &manifest))

	t.Logf("Cache manifest has %d layers", len(manifest.Layers))

	// Check each layer for foreign references
	var foreignLayers, localLayers int
	for i, layer := range manifest.Layers {
		t.Logf("Layer %d: digest=%s, size=%d, mediaType=%s", i, layer.Digest, layer.Size, layer.MediaType)

		// Check if layer has foreign URLs (indicates reference to external registry)
		if len(layer.URLs) > 0 {
			t.Logf("  Layer %d: FOREIGN - has external URLs: %v", i, layer.URLs)
			foreignLayers++
			continue
		}

		// Try to HEAD the layer blob to verify it exists locally
		layerURL := fmt.Sprintf("http://%s/v2/cache/test/alpine-cache/blobs/%s", buildkitHost, layer.Digest)
		layerResp, err := http.Head(layerURL)
		if err != nil || layerResp.StatusCode != http.StatusOK {
			t.Logf("  Layer %d: NOT FOUND in registry", i)
			foreignLayers++
		} else {
			t.Logf("  Layer %d: FOUND in registry (local blob)", i)
			localLayers++
			layerResp.Body.Close()
		}
	}

	t.Logf("Summary: %d local layers, %d foreign layers", localLayers, foreignLayers)

	// The key assertion: with image-manifest=true, ALL layers should be stored locally
	assert.Equal(t, 0, foreignLayers,
		"Cache should contain all layer blobs locally (no foreign references). "+
			"Foreign layers indicate base image layers are still referenced to Docker Hub.")
	assert.Greater(t, localLayers, 0, "Cache should have at least one local layer")
}

// TestBuildKitCacheHitForBaseImageLayers verifies that when importing a cache,
// BuildKit actually uses the cached layers for the FROM instruction.
//
// This test:
// 1. Builds an image and exports cache (simulating admin cache population)
// 2. Prunes BuildKit's local cache
// 3. Builds again with only import-cache (simulating fresh tenant build)
// 4. Analyzes output to verify cache behavior for base image layers
func TestBuildKitCacheHitForBaseImageLayers(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	ctx := context.Background()

	// Create a shared network for the containers
	testNetwork, err := network.New(ctx)
	require.NoError(t, err, "failed to create network")
	defer testNetwork.Remove(ctx)

	networkName := testNetwork.Name

	// Start registry container
	registryC, _, registryHost := startRegistryContainer(t, ctx, networkName)
	defer registryC.Terminate(ctx)

	// Start BuildKit container on the same network
	buildkitC := startBuildKitContainer(t, ctx, networkName)
	defer buildkitC.Terminate(ctx)

	// Create test Dockerfile
	srcDir := t.TempDir()
	dockerfile := `FROM alpine:3.21
RUN echo "cache test" > /test.txt
`
	require.NoError(t, os.WriteFile(filepath.Join(srcDir, "Dockerfile"), []byte(dockerfile), 0644))

	// Copy source to BuildKit container
	copyToContainer(t, ctx, buildkitC, srcDir, "/src")

	cacheRef := fmt.Sprintf("%s/cache/global/test-runtime:v1", registryHost)
	outputRef1 := fmt.Sprintf("%s/builds/admin-build:v1", registryHost)
	outputRef2 := fmt.Sprintf("%s/builds/tenant-build:v1", registryHost)

	// Step 1: Admin build - populate cache
	t.Log("Step 1: Admin build to populate cache...")
	buildCmd1 := []string{
		"buildctl", "build",
		"--frontend", "dockerfile.v0",
		"--local", "context=/src",
		"--local", "dockerfile=/src",
		"--output", fmt.Sprintf("type=image,name=%s,push=true,registry.insecure=true", outputRef1),
		"--export-cache", fmt.Sprintf("type=registry,ref=%s,mode=max,image-manifest=true,oci-mediatypes=true,registry.insecure=true", cacheRef),
	}

	exitCode, output, err := execInContainer(t, ctx, buildkitC, buildCmd1)
	t.Logf("Admin build output:\n%s", output)
	require.NoError(t, err)
	require.Equal(t, 0, exitCode, "Admin build failed")

	// Step 2: Clear BuildKit's local cache to simulate fresh/ephemeral environment
	t.Log("Step 2: Clearing BuildKit local cache...")
	pruneCmd := []string{"buildctl", "prune", "--all"}
	execInContainer(t, ctx, buildkitC, pruneCmd) // Ignore errors

	// Step 3: Tenant build - import from cache only (no export)
	t.Log("Step 3: Tenant build with cache import...")
	buildCmd2 := []string{
		"buildctl", "build",
		"--frontend", "dockerfile.v0",
		"--local", "context=/src",
		"--local", "dockerfile=/src",
		"--output", fmt.Sprintf("type=image,name=%s,push=true,registry.insecure=true", outputRef2),
		"--import-cache", fmt.Sprintf("type=registry,ref=%s,registry.insecure=true", cacheRef),
		"--progress", "plain",
	}

	exitCode, output, err = execInContainer(t, ctx, buildkitC, buildCmd2)
	buildLog := output
	t.Logf("Tenant build output:\n%s", buildLog)
	require.NoError(t, err)
	require.Equal(t, 0, exitCode, "Tenant build failed")

	// Analyze build output to determine if cache was used for base image
	t.Log("Analyzing build output for cache effectiveness...")

	// Check for actual layer download from Docker Hub (shows download progress like "3.64MB / 3.64MB")
	// Note: "resolve docker.io" is just metadata resolution (checking digest), not layer download
	hasLayerDownload := strings.Contains(buildLog, "MB /") || // Download progress indicator
		strings.Contains(buildLog, "extracting sha256:") // Layer extraction

	// Check for metadata resolution (this is expected - BuildKit needs to verify the digest)
	hasMetadataResolve := strings.Contains(buildLog, "resolve docker.io")

	// Check for CACHED indicator on steps
	hasCachedSteps := strings.Contains(buildLog, "CACHED")

	// Check for cache import messages
	hasCacheImport := strings.Contains(buildLog, "importing cache manifest")

	t.Logf("Analysis results:")
	t.Logf("  - Cache import detected: %v", hasCacheImport)
	t.Logf("  - Metadata resolution (docker.io): %v (this is normal - just checking digest)", hasMetadataResolve)
	t.Logf("  - Actual layer download detected: %v", hasLayerDownload)
	t.Logf("  - Has CACHED steps: %v", hasCachedSteps)

	// Document the expected behavior vs actual behavior
	if hasLayerDownload {
		t.Log("")
		t.Log("=== ISSUE REPRODUCED ===")
		t.Log("Layer download detected despite cache import.")
		t.Log("This confirms that BuildKit's registry cache does not effectively")
		t.Log("provide base image layers from the cache.")
		t.Log("")
	} else if hasMetadataResolve && !hasLayerDownload {
		t.Log("")
		t.Log("=== CACHE WORKING CORRECTLY ===")
		t.Log("Metadata was resolved from Docker Hub (normal behavior to verify digest),")
		t.Log("but NO layer download occurred - layers were served from cache!")
		t.Log("")
	}

	// The key assertion: check for actual layer downloads, not just metadata resolution
	// Metadata resolution is expected, but layer download should NOT happen with proper cache
	assert.False(t, hasLayerDownload,
		"Build should NOT download layers from Docker Hub when cache is available. "+
			"Layer download indicators (MB progress, extraction) should not appear.")
}

// TestCacheExportArgsFormat verifies the cache export arguments are correctly formatted.
func TestCacheExportArgsFormat(t *testing.T) {
	key := &CacheKey{
		Reference:    "localhost:5000/cache/tenant/nodejs/abc123",
		TenantScope:  "tenant",
		Runtime:      "nodejs",
		LockfileHash: "abc123",
	}

	exportArg := key.ExportCacheArg()

	// Verify all required options are present
	assert.Contains(t, exportArg, "type=registry")
	assert.Contains(t, exportArg, "ref=localhost:5000/cache/tenant/nodejs/abc123")
	assert.Contains(t, exportArg, "mode=max")
	assert.Contains(t, exportArg, "image-manifest=true")
	assert.Contains(t, exportArg, "oci-mediatypes=true")

	// Verify the exact format matches what BuildKit expects
	expected := "type=registry,ref=localhost:5000/cache/tenant/nodejs/abc123,mode=max,image-manifest=true,oci-mediatypes=true"
	assert.Equal(t, expected, exportArg)
}

// startRegistryContainer starts a Docker registry container for testing.
// Returns the container, external host (for test access), and internal host (for container-to-container access).
func startRegistryContainer(t *testing.T, ctx context.Context, networkName string) (testcontainers.Container, string, string) {
	t.Helper()

	const registryAlias = "registry"

	req := testcontainers.ContainerRequest{
		Image:        "registry:2",
		ExposedPorts: []string{"5000/tcp"},
		WaitingFor:   wait.ForHTTP("/v2/").WithPort("5000/tcp"),
		Networks:     []string{networkName},
		NetworkAliases: map[string][]string{
			networkName: {registryAlias},
		},
	}

	container, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: req,
		Started:          true,
	})
	require.NoError(t, err, "failed to start registry container")

	// Get the mapped port for external access
	mappedPort, err := container.MappedPort(ctx, "5000")
	require.NoError(t, err)

	host, err := container.Host(ctx)
	require.NoError(t, err)

	externalHost := fmt.Sprintf("%s:%s", host, mappedPort.Port())
	internalHost := fmt.Sprintf("%s:5000", registryAlias) // Container-to-container uses alias:port

	t.Logf("Registry started - external: %s, internal: %s", externalHost, internalHost)

	return container, externalHost, internalHost
}

// startBuildKitContainer starts a BuildKit container for testing on the specified network.
func startBuildKitContainer(t *testing.T, ctx context.Context, networkName string) testcontainers.Container {
	t.Helper()

	req := testcontainers.ContainerRequest{
		Image:      "moby/buildkit:latest",
		Privileged: true,
		Entrypoint: []string{"buildkitd"},
		WaitingFor: wait.ForLog("running server").WithStartupTimeout(30 * time.Second),
		Networks:   []string{networkName},
	}

	container, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: req,
		Started:          true,
	})
	require.NoError(t, err, "failed to start BuildKit container")

	t.Log("BuildKit container started")

	return container
}

// copyToContainer copies a directory to the container.
func copyToContainer(t *testing.T, ctx context.Context, container testcontainers.Container, srcPath, dstPath string) {
	t.Helper()

	// First create the destination directory
	_, _, err := container.Exec(ctx, []string{"mkdir", "-p", dstPath})
	require.NoError(t, err, "failed to create directory %s in container", dstPath)

	// Copy files individually
	files, err := os.ReadDir(srcPath)
	require.NoError(t, err, "failed to read source directory %s", srcPath)

	for _, file := range files {
		srcFile := filepath.Join(srcPath, file.Name())
		content, err := os.ReadFile(srcFile)
		require.NoError(t, err, "failed to read file %s", srcFile)

		err = container.CopyToContainer(ctx, content, filepath.Join(dstPath, file.Name()), 0644)
		require.NoError(t, err, "failed to copy %s to container", file.Name())
	}
}

// execInContainer executes a command in the container and returns exit code, output, and error.
func execInContainer(t *testing.T, ctx context.Context, container testcontainers.Container, cmd []string) (int, string, error) {
	t.Helper()

	exitCode, reader, err := container.Exec(ctx, cmd)
	if err != nil {
		return exitCode, "", err
	}

	output, _ := io.ReadAll(reader)
	return exitCode, string(output), nil
}

// TestCacheMismatchWithDifferentBaseImage demonstrates that cache populated with
// one base image does NOT help builds using a different base image.
// This reproduces the production issue where:
// - Global cache was populated with one Dockerfile (e.g., FROM node:20-alpine)
// - Tenant builds use a different base image (e.g., FROM onkernel/nodejs22-base:0.1.1)
// - Cache import succeeds but layers still download because digests don't match
func TestCacheMismatchWithDifferentBaseImage(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	ctx := context.Background()

	// Create a shared network
	testNetwork, err := network.New(ctx)
	require.NoError(t, err, "failed to create network")
	defer testNetwork.Remove(ctx)

	networkName := testNetwork.Name

	// Start registry and BuildKit
	registryC, _, registryHostInternal := startRegistryContainer(t, ctx, networkName)
	defer registryC.Terminate(ctx)

	buildkitC := startBuildKitContainer(t, ctx, networkName)
	defer buildkitC.Terminate(ctx)

	registryHost := registryHostInternal
	cacheRef := fmt.Sprintf("%s/cache/global/node:v1", registryHost)

	// Step 1: Populate cache with alpine:3.21 (simulating admin build with different base)
	t.Log("Step 1: Populating cache with alpine:3.21 (different base image)...")
	srcDir1 := t.TempDir()
	dockerfile1 := `FROM alpine:3.21
RUN echo "admin build" > /admin.txt
`
	require.NoError(t, os.WriteFile(filepath.Join(srcDir1, "Dockerfile"), []byte(dockerfile1), 0644))
	copyToContainer(t, ctx, buildkitC, srcDir1, "/src1")

	buildCmd1 := []string{
		"buildctl", "build",
		"--frontend", "dockerfile.v0",
		"--local", "context=/src1",
		"--local", "dockerfile=/src1",
		"--output", fmt.Sprintf("type=image,name=%s/builds/admin:v1,push=true,registry.insecure=true", registryHost),
		"--export-cache", fmt.Sprintf("type=registry,ref=%s,mode=max,image-manifest=true,oci-mediatypes=true,registry.insecure=true", cacheRef),
	}

	exitCode, output, err := execInContainer(t, ctx, buildkitC, buildCmd1)
	t.Logf("Admin build output:\n%s", output)
	require.NoError(t, err)
	require.Equal(t, 0, exitCode)

	// Step 2: Clear local cache
	t.Log("Step 2: Clearing local cache...")
	execInContainer(t, ctx, buildkitC, []string{"buildctl", "prune", "--all"})

	// Step 3: Tenant build with DIFFERENT base image (alpine:3.20 instead of 3.21)
	t.Log("Step 3: Tenant build with DIFFERENT base image (alpine:3.20)...")
	srcDir2 := t.TempDir()
	dockerfile2 := `FROM alpine:3.20
RUN echo "tenant build" > /tenant.txt
`
	require.NoError(t, os.WriteFile(filepath.Join(srcDir2, "Dockerfile"), []byte(dockerfile2), 0644))
	copyToContainer(t, ctx, buildkitC, srcDir2, "/src2")

	buildCmd2 := []string{
		"buildctl", "build",
		"--frontend", "dockerfile.v0",
		"--local", "context=/src2",
		"--local", "dockerfile=/src2",
		"--output", fmt.Sprintf("type=image,name=%s/builds/tenant:v1,push=true,registry.insecure=true", registryHost),
		"--import-cache", fmt.Sprintf("type=registry,ref=%s,registry.insecure=true", cacheRef),
		"--progress", "plain",
	}

	exitCode, output, err = execInContainer(t, ctx, buildkitC, buildCmd2)
	buildLog := output
	t.Logf("Tenant build output:\n%s", buildLog)
	require.NoError(t, err)
	require.Equal(t, 0, exitCode)

	// Analyze: cache import works, but layers still download
	hasCacheImport := strings.Contains(buildLog, "importing cache manifest")
	hasLayerDownload := strings.Contains(buildLog, "MB /") || strings.Contains(buildLog, "extracting sha256:")

	t.Logf("Analysis:")
	t.Logf("  - Cache import: %v", hasCacheImport)
	t.Logf("  - Layer download: %v", hasLayerDownload)

	// This demonstrates the problem: cache imports successfully but layers still download
	// because the base images are different (alpine:3.21 vs alpine:3.20)
	assert.True(t, hasCacheImport, "Cache manifest should be imported")
	assert.True(t, hasLayerDownload,
		"EXPECTED: Layers SHOULD download because base image differs. "+
			"This demonstrates that cache populated with one base image doesn't help "+
			"builds using a different base image.")

	t.Log("")
	t.Log("=== TEST DEMONSTRATES THE PRODUCTION ISSUE ===")
	t.Log("Cache was imported successfully, but layers still downloaded because")
	t.Log("the cached layers (from alpine:3.21) don't match the required layers")
	t.Log("(from alpine:3.20). The layer digests are completely different.")
	t.Log("")
	t.Log("FIX: Ensure populate-global-cache uses the SAME base image that")
	t.Log("tenant Dockerfiles use (e.g., onkernel/nodejs22-base:0.1.1)")
}

// TestInspectCacheManifestStructure inspects the structure of a BuildKit cache manifest
// to understand what's being stored.
func TestInspectCacheManifestStructure(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	ctx := context.Background()

	// Create a shared network for the containers
	testNetwork, err := network.New(ctx)
	require.NoError(t, err, "failed to create network")
	defer testNetwork.Remove(ctx)

	networkName := testNetwork.Name

	// Start registry container
	registryC, registryHostExternal, registryHostInternal := startRegistryContainer(t, ctx, networkName)
	defer registryC.Terminate(ctx)

	// Start BuildKit container on the same network
	buildkitC := startBuildKitContainer(t, ctx, networkName)
	defer buildkitC.Terminate(ctx)

	// Use internal host for BuildKit, external for test HTTP requests
	registryHost := registryHostInternal
	buildkitHostForRegistry := registryHostExternal

	// Create simple Dockerfile
	srcDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(srcDir, "Dockerfile"), []byte(`FROM alpine:3.21
RUN echo "test" > /test.txt
`), 0644))

	copyToContainer(t, ctx, buildkitC, srcDir, "/src")

	cacheRef := fmt.Sprintf("%s/cache/inspect-test:v1", registryHost)
	outputRef := fmt.Sprintf("%s/builds/inspect-test:v1", registryHost)

	// Build with cache export
	buildCmd := []string{
		"buildctl", "build",
		"--frontend", "dockerfile.v0",
		"--local", "context=/src",
		"--local", "dockerfile=/src",
		"--output", fmt.Sprintf("type=image,name=%s,push=true,registry.insecure=true", outputRef),
		"--export-cache", fmt.Sprintf("type=registry,ref=%s,mode=max,image-manifest=true,oci-mediatypes=true,registry.insecure=true", cacheRef),
	}

	exitCode, output, err := execInContainer(t, ctx, buildkitC, buildCmd)
	t.Logf("Build output:\n%s", output)
	require.NoError(t, err)
	require.Equal(t, 0, exitCode)

	// Fetch and inspect the cache manifest
	manifestURL := fmt.Sprintf("http://%s/v2/cache/inspect-test/manifests/v1", buildkitHostForRegistry)
	req, _ := http.NewRequest("GET", manifestURL, nil)
	req.Header.Set("Accept", "application/vnd.oci.image.manifest.v1+json, application/vnd.docker.distribution.manifest.v2+json, application/vnd.oci.image.index.v1+json")

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	t.Logf("Cache manifest (status %d):\n%s", resp.StatusCode, string(body))

	if resp.StatusCode == http.StatusOK {
		var manifest map[string]interface{}
		if err := json.Unmarshal(body, &manifest); err == nil {
			t.Logf("Manifest mediaType: %s", manifest["mediaType"])
			if layers, ok := manifest["layers"].([]interface{}); ok {
				t.Logf("Number of layers: %d", len(layers))
				for i, layer := range layers {
					if l, ok := layer.(map[string]interface{}); ok {
						t.Logf("  Layer %d: mediaType=%s, size=%v, digest=%s",
							i, l["mediaType"], l["size"], l["digest"])
						// Check for foreign layer URLs
						if urls, ok := l["urls"]; ok {
							t.Logf("    WARNING: Layer has external URLs: %v", urls)
						}
					}
				}
			}
		}
	}
}
