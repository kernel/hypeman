package images

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/layout"
	"github.com/google/go-containerregistry/pkg/v1/remote"
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
	ref, err := name.ParseReference(imageRef)
	if err != nil {
		return "", fmt.Errorf("parse image reference: %w", err)
	}

	// Use system authentication (reads from ~/.docker/config.json, etc.)
	// Default retry: only on network errors, max ~1.3s total
	descriptor, err := remote.Head(ref, 
		remote.WithContext(ctx),
		remote.WithAuthFromKeychain(authn.DefaultKeychain))
	if err != nil {
		return "", fmt.Errorf("fetch manifest: %w", wrapRegistryError(err))
	}

	return descriptor.Digest.String(), nil
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
	ref, err := name.ParseReference(imageRef)
	if err != nil {
		return fmt.Errorf("parse image reference: %w", err)
	}

	// Use system authentication (reads from ~/.docker/config.json, etc.)
	// Default retry: only on network errors, max ~1.3s total
	img, err := remote.Image(ref, 
		remote.WithContext(ctx),
		remote.WithAuthFromKeychain(authn.DefaultKeychain))
	if err != nil {
		// Rate limits fail here immediately (429 is not retried by default)
		return fmt.Errorf("fetch image manifest: %w", wrapRegistryError(err))
	}

	// Open or create OCI layout directory
	path, err := layout.FromPath(c.cacheDir)
	if err != nil {
		// If layout doesn't exist, create it
		path, err = layout.Write(c.cacheDir, empty.Index)
		if err != nil {
			return fmt.Errorf("create oci layout: %w", err)
		}
	}

	// Append image to layout - THIS is where actual layer data is downloaded
	// Streams layers from registry and writes to blobs/sha256/ directory
	// Automatically deduplicates shared layers across images
	// Rate limits during layer download also fail immediately (no retries)
	err = path.AppendImage(img, layout.WithAnnotations(map[string]string{
		"org.opencontainers.image.ref.name": layoutTag,
	}))
	if err != nil {
		return fmt.Errorf("download and write image layers: %w", err)
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

