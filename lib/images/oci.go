package images

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	gcr "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/layout"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	digest "github.com/opencontainers/go-digest"
	"github.com/opencontainers/image-spec/specs-go"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	rspec "github.com/opencontainers/runtime-spec/specs-go"
	"github.com/opencontainers/umoci/oci/cas/dir"
	"github.com/opencontainers/umoci/oci/casext"
	"github.com/opencontainers/umoci/oci/layer"
)

// ociClient handles OCI image operations without requiring Docker daemon
type ociClient struct {
	cacheDir string
	keychain authn.Keychain
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

// newOCIClient creates a new OCI client. If keychain is nil, the Docker
// config keychain is used for registry authentication.
func newOCIClient(cacheDir string, keychain authn.Keychain) (*ociClient, error) {
	if err := os.MkdirAll(cacheDir, 0755); err != nil {
		return nil, fmt.Errorf("create cache dir: %w", err)
	}
	if keychain == nil {
		keychain = authn.DefaultKeychain
	}
	return &ociClient{cacheDir: cacheDir, keychain: keychain}, nil
}

// vmPlatform returns the target platform for VM images: a Linux guest on the
// host architecture. Hypeman VMs are always Linux regardless of host OS.
func vmPlatform() gcr.Platform {
	return hostPlatform().ToGCR()
}

// inspectManifest synchronously inspects a remote image to get its digest
// without pulling the image. This is used for upfront digest discovery.
// For multi-arch images, it returns the platform-specific manifest digest
// (matching the current host platform) rather than the manifest index digest.
func (c *ociClient) inspectManifest(ctx context.Context, imageRef string) (string, error) {
	return c.inspectManifestWithPlatform(ctx, imageRef, vmPlatform())
}

// inspectManifestWithPlatform synchronously inspects a remote image to get its digest
// for a specific platform.
func (c *ociClient) inspectManifestWithPlatform(ctx context.Context, imageRef string, platform gcr.Platform) (string, error) {
	ref, err := name.ParseReference(imageRef)
	if err != nil {
		return "", fmt.Errorf("parse image reference: %w", err)
	}

	// Use remote.Image with platform filtering to get the platform-specific digest.
	// For multi-arch images, this resolves the manifest index to the correct platform.
	// This matches what pullToOCILayout does to ensure cache key consistency.
	// Note: remote.Image is lazy - it only fetches the manifest, not layer blobs.
	//
	// Registry-error classification (rate-limit, not-found, platform-not-available)
	// is centralized here so callers can `%w` the result without re-wrapping.
	img, err := remote.Image(ref,
		remote.WithContext(ctx),
		remote.WithAuthFromKeychain(c.keychain),
		remote.WithPlatform(platform))
	if err != nil {
		return "", fmt.Errorf("fetch manifest: %w", wrapRegistryError(err))
	}

	// remote.Image is lazy, so the "no child with platform" error for a multi-arch
	// index surfaces here rather than from remote.Image above; classify it too.
	digest, err := img.Digest()
	if err != nil {
		return "", fmt.Errorf("get image digest: %w", wrapRegistryError(err))
	}

	return digest.String(), nil
}

// inspectDigestPlatform resolves a digest-pinned reference to a concrete image
// for the requested platform and returns both the platform that image's config
// declares and the resolved child digest. It is used to validate a user-supplied
// --platform against a digest ref and to record the resolved (child) digest.
//
// The pinned digest can name either a single-arch image manifest or a multi-arch
// index (manifest list). These need opposite handling, which is why we fetch the
// descriptor first and branch on its media type:
//   - For an image manifest, the digest IS the architecture; WithPlatform must
//     NOT be passed, since go-containerregistry would re-resolve the parent index
//     to a host-matching child and the requested platform would never be checked.
//   - For an index, WithPlatform IS correct: it selects the child for the
//     requested platform (a missing child surfaces as "no child with platform"
//     -> ErrPlatformNotAvailable -> 404 via wrapRegistryError).
//
// A digest absent from the registry surfaces as a not-found error, classified by
// wrapRegistryError.
func (c *ociClient) inspectDigestPlatform(ctx context.Context, imageRef string, requested gcr.Platform) (Platform, string, error) {
	ref, err := name.ParseReference(imageRef)
	if err != nil {
		return Platform{}, "", fmt.Errorf("parse image reference: %w", err)
	}

	desc, err := remote.Get(ref,
		remote.WithContext(ctx),
		remote.WithAuthFromKeychain(c.keychain))
	if err != nil {
		return Platform{}, "", fmt.Errorf("fetch manifest: %w", wrapRegistryError(err))
	}

	var img gcr.Image
	if desc.MediaType.IsIndex() {
		// Index: select the child for the requested platform. WithPlatform on the
		// re-parsed digest ref is correct here because the digest names the index.
		img, err = remote.Image(ref,
			remote.WithContext(ctx),
			remote.WithAuthFromKeychain(c.keychain),
			remote.WithPlatform(requested))
	} else {
		// Single-arch manifest: the digest already pins the exact image.
		img, err = desc.Image()
	}
	if err != nil {
		return Platform{}, "", fmt.Errorf("resolve image for platform: %w", wrapRegistryError(err))
	}

	configFile, err := img.ConfigFile()
	if err != nil {
		return Platform{}, "", fmt.Errorf("get image config: %w", wrapRegistryError(err))
	}

	resolvedDigest, err := img.Digest()
	if err != nil {
		return Platform{}, "", fmt.Errorf("get image digest: %w", wrapRegistryError(err))
	}

	return Platform{
		OS:           configFile.OS,
		Architecture: configFile.Architecture,
		Variant:      configFile.Variant,
	}.Normalize(), resolvedDigest.String(), nil
}

// pullResult contains the metadata and digest from pulling an image
type pullResult struct {
	Metadata *containerMetadata
	Digest   string // sha256:abc123...
}

func (c *ociClient) pullAndExport(ctx context.Context, imageRef, digest, exportDir string) (*pullResult, error) {
	return c.pullAndExportWithPlatform(ctx, imageRef, digest, exportDir, vmPlatform())
}

func (c *ociClient) pullAndExportWithPlatform(ctx context.Context, imageRef, digest, exportDir string, platform gcr.Platform) (*pullResult, error) {
	// Use a shared OCI layout for all images to enable automatic layer caching
	// The cacheDir itself is the OCI layout root with shared blobs/sha256/ directory
	// The digest is ALWAYS known at this point (from inspectManifest or digest reference)
	layoutTag := digestToLayoutTag(digest)

	// Check if this digest is already cached
	if !c.existsInLayout(layoutTag) {
		// Not cached, pull it using digest-based tag
		if err := c.pullToOCILayoutWithPlatform(ctx, imageRef, layoutTag, platform); err != nil {
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
	return c.pullToOCILayoutWithPlatform(ctx, imageRef, layoutTag, vmPlatform())
}

func (c *ociClient) pullToOCILayoutWithPlatform(ctx context.Context, imageRef, layoutTag string, platform gcr.Platform) error {
	ref, err := name.ParseReference(imageRef)
	if err != nil {
		return fmt.Errorf("parse image reference: %w", err)
	}

	// Use system authentication (reads from ~/.docker/config.json, etc.)
	// Default retry: only on network errors, max ~1.3s total
	// WithPlatform ensures we pull the correct architecture for multi-arch images
	img, err := remote.Image(ref,
		remote.WithContext(ctx),
		remote.WithAuthFromKeychain(c.keychain),
		remote.WithPlatform(platform))
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

// imageByAnnotation finds an image in the OCI layout by its annotation tag.
// This iterates through the index to find the image with matching
// "org.opencontainers.image.ref.name" annotation.
func imageByAnnotation(path layout.Path, layoutTag string) (gcr.Image, error) {
	index, err := path.ImageIndex()
	if err != nil {
		return nil, fmt.Errorf("get image index: %w", err)
	}

	indexManifest, err := index.IndexManifest()
	if err != nil {
		return nil, fmt.Errorf("get index manifest: %w", err)
	}

	// Find the image with matching annotation
	for _, desc := range indexManifest.Manifests {
		if desc.Annotations != nil {
			if refName, ok := desc.Annotations["org.opencontainers.image.ref.name"]; ok {
				if refName == layoutTag {
					return path.Image(desc.Digest)
				}
			}
		}
	}

	return nil, fmt.Errorf("no image found with tag %s", layoutTag)
}

// extractOCIMetadata reads metadata from OCI layout config.json
// Uses go-containerregistry which handles both Docker v2 and OCI v1 manifests.
func (c *ociClient) extractOCIMetadata(layoutTag string) (*containerMetadata, error) {
	// Open OCI layout using go-containerregistry (handles Docker v2 and OCI v1)
	path, err := layout.FromPath(c.cacheDir)
	if err != nil {
		return nil, fmt.Errorf("open oci layout: %w", err)
	}

	// Get the image by annotation tag from the layout
	img, err := imageByAnnotation(path, layoutTag)
	if err != nil {
		return nil, fmt.Errorf("find image by tag %s: %w", layoutTag, err)
	}

	// Get config file (go-containerregistry handles manifest format automatically)
	configFile, err := img.ConfigFile()
	if err != nil {
		return nil, fmt.Errorf("get config file: %w", err)
	}

	// Extract metadata from config. OS/Architecture/Variant come straight from
	// the pulled image config, so they reflect the manifest actually fetched
	// rather than what the caller requested.
	meta := &containerMetadata{
		OS:           configFile.OS,
		Architecture: configFile.Architecture,
		Variant:      configFile.Variant,
		Entrypoint:   configFile.Config.Entrypoint,
		Cmd:          configFile.Config.Cmd,
		Env:          make(map[string]string),
		Labels:       make(map[string]string),
		WorkingDir:   configFile.Config.WorkingDir,
	}

	// Parse environment variables
	for _, env := range configFile.Config.Env {
		for i := 0; i < len(env); i++ {
			if env[i] == '=' {
				key := env[:i]
				val := env[i+1:]
				meta.Env[key] = val
				break
			}
		}
	}

	for key, value := range configFile.Config.Labels {
		meta.Labels[key] = value
	}

	return meta, nil
}

// unpackLayers unpacks all OCI layers to a target directory using umoci
// Uses go-containerregistry to get the manifest (handles both Docker v2 and OCI v1)
// then converts it to OCI v1 format for umoci's layer unpacker.
func (c *ociClient) unpackLayers(ctx context.Context, layoutTag, targetDir string) error {
	// Open OCI layout using go-containerregistry (handles Docker v2 and OCI v1)
	path, err := layout.FromPath(c.cacheDir)
	if err != nil {
		return fmt.Errorf("open oci layout: %w", err)
	}

	// Get the image by annotation tag from the layout
	img, err := imageByAnnotation(path, layoutTag)
	if err != nil {
		return fmt.Errorf("find image by tag %s: %w", layoutTag, err)
	}

	// Get manifest from go-containerregistry
	gcrManifest, err := img.Manifest()
	if err != nil {
		return fmt.Errorf("get manifest: %w", err)
	}

	configFile, err := img.ConfigFile()
	if err != nil {
		return fmt.Errorf("get config file: %w", err)
	}
	if err := validateConfigFileForUnpack(layoutTag, gcrManifest, configFile); err != nil {
		return err
	}

	// Convert go-containerregistry manifest to OCI v1.Manifest for umoci
	ociManifest := convertToOCIManifest(gcrManifest)

	// Open the shared OCI layout with umoci for layer unpacking
	casEngine, err := dir.Open(c.cacheDir)
	if err != nil {
		return fmt.Errorf("open oci layout for unpacking: %w", err)
	}
	defer casEngine.Close()

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

	err = layer.UnpackRootfs(ctx, casEngine, targetDir, ociManifest, unpackOpts)
	if err != nil {
		return fmt.Errorf("unpack rootfs: %w", err)
	}

	return nil
}

// validateConfigFileForUnpack rejects malformed image configs before calling
// umoci. In particular, we verify that the config blob resolves to a real OCI
// image config, that it declares a layered rootfs, and that rootfs.diff_ids has
// one entry per manifest layer so umoci won't index past the end of the slice.
func validateConfigFileForUnpack(layoutTag string, manifest *gcr.Manifest, configFile *gcr.ConfigFile) error {
	if convertToOCIMediaType(string(manifest.Config.MediaType)) != v1.MediaTypeImageConfig {
		return fmt.Errorf(
			"unpack rootfs: config blob is not correct mediatype %s: %s",
			v1.MediaTypeImageConfig,
			manifest.Config.MediaType,
		)
	}
	if configFile.RootFS.Type != "layers" {
		return fmt.Errorf("unpack rootfs: config: unsupported rootfs.type: %s", configFile.RootFS.Type)
	}
	if len(configFile.RootFS.DiffIDs) != len(manifest.Layers) {
		return fmt.Errorf(
			"unpack rootfs: config rootfs.diff_ids has %d entries but manifest has %d layers for %s",
			len(configFile.RootFS.DiffIDs),
			len(manifest.Layers),
			layoutTag,
		)
	}
	return nil
}

// convertToOCIManifest converts a go-containerregistry manifest to OCI v1.Manifest
// This allows us to use go-containerregistry (which handles both Docker v2 and OCI v1)
// for manifest parsing, while still using umoci for layer unpacking.
// Docker v2 mediatypes are converted to OCI equivalents since umoci expects OCI format.
func convertToOCIManifest(gcrManifest *gcr.Manifest) v1.Manifest {
	// Convert config descriptor with mediatype conversion
	configDesc := v1.Descriptor{
		MediaType:   convertToOCIMediaType(string(gcrManifest.Config.MediaType)),
		Digest:      gcrDigestToOCI(gcrManifest.Config.Digest),
		Size:        gcrManifest.Config.Size,
		Annotations: gcrManifest.Config.Annotations,
	}

	// Convert layer descriptors with mediatype conversion
	layers := make([]v1.Descriptor, len(gcrManifest.Layers))
	for i, layer := range gcrManifest.Layers {
		layers[i] = v1.Descriptor{
			MediaType:   convertToOCIMediaType(string(layer.MediaType)),
			Digest:      gcrDigestToOCI(layer.Digest),
			Size:        layer.Size,
			Annotations: layer.Annotations,
		}
	}

	return v1.Manifest{
		Versioned: specs.Versioned{
			SchemaVersion: int(gcrManifest.SchemaVersion),
		},
		MediaType:   convertToOCIMediaType(string(gcrManifest.MediaType)),
		Config:      configDesc,
		Layers:      layers,
		Annotations: gcrManifest.Annotations,
	}
}

// convertToOCIMediaType converts Docker v2 media types to OCI equivalents.
// Images from Docker Hub often use Docker-specific mediatypes, but umoci
// requires OCI-standard mediatypes for layer unpacking.
func convertToOCIMediaType(mediaType string) string {
	switch mediaType {
	case "application/vnd.docker.distribution.manifest.v2+json":
		return v1.MediaTypeImageManifest
	case "application/vnd.docker.container.image.v1+json":
		return v1.MediaTypeImageConfig
	case "application/vnd.docker.image.rootfs.diff.tar.gzip":
		return v1.MediaTypeImageLayerGzip
	case "application/vnd.docker.image.rootfs.diff.tar":
		return v1.MediaTypeImageLayer
	default:
		// If already OCI or unknown, return as-is
		return mediaType
	}
}

// gcrDigestToOCI converts a go-containerregistry digest to OCI digest
func gcrDigestToOCI(d gcr.Hash) digest.Digest {
	return digest.NewDigestFromEncoded(digest.Algorithm(d.Algorithm), d.Hex)
}

type containerMetadata struct {
	OS           string
	Architecture string
	Variant      string
	Entrypoint   []string
	Cmd          []string
	Env          map[string]string
	Labels       map[string]string
	WorkingDir   string
}
