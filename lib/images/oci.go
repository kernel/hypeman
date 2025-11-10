package images

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/containers/image/v5/copy"
	"github.com/containers/image/v5/docker"
	"github.com/containers/image/v5/manifest"
	"github.com/containers/image/v5/oci/layout"
	"github.com/containers/image/v5/signature"
	"github.com/opencontainers/image-spec/specs-go/v1"
	rspec "github.com/opencontainers/runtime-spec/specs-go"
	"github.com/opencontainers/umoci/oci/cas/dir"
	"github.com/opencontainers/umoci/oci/casext"
	"github.com/opencontainers/umoci/oci/layer"
)

// ociClient handles OCI image operations without requiring Docker daemon
type ociClient struct {
	cacheDir string
}

// digestToLayoutTag converts a digest to a valid OCI layout tag.
// Uses just the hex portion without the algorithm prefix.
// Example: "sha256:abc123..." -> "abc123..."
func digestToLayoutTag(digest string) string {
	// Extract just the hex hash after the colon
	parts := strings.SplitN(digest, ":", 2)
	if len(parts) == 2 {
		return parts[1]
	}
	return digest // Fallback if no colon found
}

// existsInLayout checks if a digest already exists in the OCI layout cache.
func (c *ociClient) existsInLayout(layoutTag string) bool {
	casEngine, err := dir.Open(c.cacheDir)
	if err != nil {
		return false
	}
	defer casEngine.Close()

	engine := casext.NewEngine(casEngine)
	descriptorPaths, err := engine.ResolveReference(context.Background(), layoutTag)
	if err != nil {
		return false
	}

	return len(descriptorPaths) > 0
}

// newOCIClient creates a new OCI client
func newOCIClient(cacheDir string) (*ociClient, error) {
	if err := os.MkdirAll(cacheDir, 0755); err != nil {
		return nil, fmt.Errorf("create cache dir: %w", err)
	}
	return &ociClient{cacheDir: cacheDir}, nil
}

// inspectManifest synchronously inspects a remote image to get its digest
// without pulling the image. This is used for upfront digest discovery.
func (c *ociClient) inspectManifest(ctx context.Context, imageRef string) (string, error) {
	srcRef, err := docker.ParseReference("//" + imageRef)
	if err != nil {
		return "", fmt.Errorf("parse image reference: %w", err)
	}

	// Create image source to inspect the remote manifest
	src, err := srcRef.NewImageSource(ctx, nil)
	if err != nil {
		return "", fmt.Errorf("create image source: %w", err)
	}
	defer src.Close()

	// Get the manifest bytes
	manifestBytes, manifestType, err := src.GetManifest(ctx, nil)
	if err != nil {
		return "", fmt.Errorf("get manifest: %w", err)
	}

	// Compute digest of the manifest
	// For multi-arch images, this returns the manifest list digest
	manifestDigest, err := manifest.Digest(manifestBytes)
	if err != nil {
		return "", fmt.Errorf("compute manifest digest: %w", err)
	}

	// Note: manifestType tells us if this is a manifest list or single-platform manifest
	_ = manifestType

	return manifestDigest.String(), nil
}

// pullResult contains the metadata and digest from pulling an image
type pullResult struct {
	Metadata *containerMetadata
	Digest   string // sha256:abc123...
}

func (c *ociClient) pullAndExport(ctx context.Context, imageRef, digest, exportDir string) (*pullResult, error) {
	// Use a shared OCI layout for all images to enable automatic layer caching
	// The cacheDir itself is the OCI layout root with shared blobs/sha256/ directory
	// The digest is ALWAYS known at this point (from inspectManifest or digest reference)
	layoutTag := digestToLayoutTag(digest)

	// Check if this digest is already cached
	if !c.existsInLayout(layoutTag) {
		// Not cached, pull it using digest-based tag
		if err := c.pullToOCILayout(ctx, imageRef, layoutTag); err != nil {
			return nil, fmt.Errorf("pull to oci layout: %w", err)
		}
	}
	// If cached, we skip the pull entirely

	// Extract metadata (from cache or freshly pulled)
	meta, err := c.extractOCIMetadata(layoutTag)
	if err != nil {
		return nil, fmt.Errorf("extract metadata: %w", err)
	}

	// Unpack layers to the export directory
	if err := c.unpackLayers(ctx, layoutTag, exportDir); err != nil {
		return nil, fmt.Errorf("unpack layers: %w", err)
	}

	return &pullResult{
		Metadata: meta,
		Digest:   digest,
	}, nil
}

func (c *ociClient) pullToOCILayout(ctx context.Context, imageRef, layoutTag string) error {
	// Parse source reference (docker://...)
	srcRef, err := docker.ParseReference("//" + imageRef)
	if err != nil {
		return fmt.Errorf("parse image reference: %w", err)
	}

	// Create destination reference (shared OCI layout with sanitized tag)
	// This allows multiple images to coexist in the same layout with automatic layer deduplication
	destRef, err := layout.ParseReference(c.cacheDir + ":" + layoutTag)
	if err != nil {
		return fmt.Errorf("parse oci layout reference: %w", err)
	}

	// Create policy context (allow all)
	policyContext, err := signature.NewPolicyContext(&signature.Policy{
		Default: []signature.PolicyRequirement{signature.NewPRInsecureAcceptAnything()},
	})
	if err != nil {
		return fmt.Errorf("create policy context: %w", err)
	}
	defer policyContext.Destroy()

	_, err = copy.Image(ctx, policyContext, destRef, srcRef, &copy.Options{
		ReportWriter: os.Stdout,
	})
	if err != nil {
		return fmt.Errorf("copy image: %w", err)
	}

	return nil
}

// extractDigest gets the manifest digest from the OCI layout
func (c *ociClient) extractDigest(layoutTag string) (string, error) {
	casEngine, err := dir.Open(c.cacheDir)
	if err != nil {
		return "", fmt.Errorf("open oci layout: %w", err)
	}
	defer casEngine.Close()

	engine := casext.NewEngine(casEngine)

	// Resolve the layout tag in the shared layout
	descriptorPaths, err := engine.ResolveReference(context.Background(), layoutTag)
	if err != nil {
		return "", fmt.Errorf("resolve reference: %w", err)
	}

	if len(descriptorPaths) == 0 {
		return "", fmt.Errorf("no image found in oci layout")
	}

	// Get the manifest descriptor's digest
	digest := descriptorPaths[0].Descriptor().Digest.String()
	return digest, nil
}

// extractOCIMetadata reads metadata from OCI layout config.json
func (c *ociClient) extractOCIMetadata(layoutTag string) (*containerMetadata, error) {
	// Open the shared OCI layout
	casEngine, err := dir.Open(c.cacheDir)
	if err != nil {
		return nil, fmt.Errorf("open oci layout: %w", err)
	}
	defer casEngine.Close()

	engine := casext.NewEngine(casEngine)

	// Resolve the layout tag in the shared layout
	descriptorPaths, err := engine.ResolveReference(context.Background(), layoutTag)
	if err != nil {
		return nil, fmt.Errorf("resolve reference: %w", err)
	}

	if len(descriptorPaths) == 0 {
		return nil, fmt.Errorf("no image found in oci layout")
	}

	// Get the manifest
	manifestBlob, err := engine.FromDescriptor(context.Background(), descriptorPaths[0].Descriptor())
	if err != nil {
		return nil, fmt.Errorf("get manifest: %w", err)
	}

	// casext automatically parses manifests, so Data is already a v1.Manifest
	manifest, ok := manifestBlob.Data.(v1.Manifest)
	if !ok {
		return nil, fmt.Errorf("manifest data is not v1.Manifest (got %T)", manifestBlob.Data)
	}

	// Get the config blob
	configBlob, err := engine.FromDescriptor(context.Background(), manifest.Config)
	if err != nil {
		return nil, fmt.Errorf("get config: %w", err)
	}

	// casext automatically parses config, so Data is already a v1.Image
	config, ok := configBlob.Data.(v1.Image)
	if !ok {
		return nil, fmt.Errorf("config data is not v1.Image (got %T)", configBlob.Data)
	}

	// Extract metadata
	meta := &containerMetadata{
		Entrypoint: config.Config.Entrypoint,
		Cmd:        config.Config.Cmd,
		Env:        make(map[string]string),
		WorkingDir: config.Config.WorkingDir,
	}

	// Parse environment variables
	for _, env := range config.Config.Env {
		for i := 0; i < len(env); i++ {
			if env[i] == '=' {
				key := env[:i]
				val := env[i+1:]
				meta.Env[key] = val
				break
			}
		}
	}

	return meta, nil
}

// unpackLayers unpacks all OCI layers to a target directory using umoci
func (c *ociClient) unpackLayers(ctx context.Context, imageRef, targetDir string) error {
	// Open the shared OCI layout
	casEngine, err := dir.Open(c.cacheDir)
	if err != nil {
		return fmt.Errorf("open oci layout: %w", err)
	}
	defer casEngine.Close()

	engine := casext.NewEngine(casEngine)

	// Resolve the image reference (tag) in the shared layout
	descriptorPaths, err := engine.ResolveReference(context.Background(), imageRef)
	if err != nil {
		return fmt.Errorf("resolve reference: %w", err)
	}

	if len(descriptorPaths) == 0 {
		return fmt.Errorf("no image found")
	}

	// Get the manifest blob
	manifestBlob, err := engine.FromDescriptor(context.Background(), descriptorPaths[0].Descriptor())
	if err != nil {
		return fmt.Errorf("get manifest: %w", err)
	}

	// casext automatically parses manifests
	manifest, ok := manifestBlob.Data.(v1.Manifest)
	if !ok {
		return fmt.Errorf("manifest data is not v1.Manifest (got %T)", manifestBlob.Data)
	}

	// Pre-create target directory (umoci needs it to exist)
	if err := os.MkdirAll(targetDir, 0755); err != nil {
		return fmt.Errorf("create target dir: %w", err)
	}

	// Unpack layers using umoci's layer package with rootless mode
	// Map container UIDs to current user's UID (identity mapping)
	uid := uint32(os.Getuid())
	gid := uint32(os.Getgid())
	
	unpackOpts := &layer.UnpackOptions{
		OnDiskFormat: layer.DirRootfs{
			MapOptions: layer.MapOptions{
				Rootless: true, // Don't fail on chown errors
				UIDMappings: []rspec.LinuxIDMapping{
					{HostID: uid, ContainerID: 0, Size: 1}, // Map container root to current user
				},
				GIDMappings: []rspec.LinuxIDMapping{
					{HostID: gid, ContainerID: 0, Size: 1}, // Map container root group to current user group
				},
			},
		},
	}
	
	err = layer.UnpackRootfs(context.Background(), casEngine, targetDir, manifest, unpackOpts)
	if err != nil {
		return fmt.Errorf("unpack rootfs: %w", err)
	}

	return nil
}

type containerMetadata struct {
	Entrypoint []string
	Cmd        []string
	Env        map[string]string
	WorkingDir string
}

