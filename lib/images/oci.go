package images

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
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

// vmPlatform returns the target platform for VM images.
// Always returns Linux since hypeman VMs are always Linux guests,
// regardless of the host OS (Linux or macOS).
func vmPlatform() gcr.Platform {
	return gcr.Platform{
		Architecture: runtime.GOARCH,
		OS:           "linux",
	}
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
	img, err := remote.Image(ref,
		remote.WithContext(ctx),
		remote.WithAuthFromKeychain(authn.DefaultKeychain),
		remote.WithPlatform(platform))
	if err != nil {
		return "", fmt.Errorf("fetch manifest: %w", wrapRegistryError(err))
	}

	digest, err := img.Digest()
	if err != nil {
		return "", fmt.Errorf("get image digest: %w", err)
	}

	return digest.String(), nil
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
		remote.WithAuthFromKeychain(authn.DefaultKeychain),
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

	// Extract metadata from config
	meta := &containerMetadata{
		Entrypoint: configFile.Config.Entrypoint,
		Cmd:        configFile.Config.Cmd,
		Env:        make(map[string]string),
		WorkingDir: configFile.Config.WorkingDir,
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

	err = layer.UnpackRootfs(context.Background(), casEngine, targetDir, ociManifest, unpackOpts)
	if err != nil {
		return fmt.Errorf("unpack rootfs: %w", err)
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
	Entrypoint []string
	Cmd        []string
	Env        map[string]string
	WorkingDir string
}

// streamingUnpack extracts layers directly from registry to target directory
// without writing to the OCI cache first. This is faster for one-time conversions
// (like building images from a local registry) because it eliminates the
// cache write/read cycle.
//
// The flow is: Registry Blob -> HTTP -> go-containerregistry -> tar extraction -> rootfs/
// vs traditional: Registry Blob -> HTTP -> OCI Cache -> umoci -> rootfs/
func (c *ociClient) streamingUnpack(ctx context.Context, imageRef string, targetDir string) (*pullResult, error) {
	return c.streamingUnpackWithPlatform(ctx, imageRef, targetDir, vmPlatform())
}

func (c *ociClient) streamingUnpackWithPlatform(ctx context.Context, imageRef string, targetDir string, platform gcr.Platform) (*pullResult, error) {
	ref, err := name.ParseReference(imageRef)
	if err != nil {
		return nil, fmt.Errorf("parse image reference: %w", err)
	}

	// Fetch image from registry (lazy - doesn't download layer blobs yet)
	img, err := remote.Image(ref,
		remote.WithContext(ctx),
		remote.WithAuthFromKeychain(authn.DefaultKeychain),
		remote.WithPlatform(platform))
	if err != nil {
		return nil, fmt.Errorf("fetch image manifest: %w", wrapRegistryError(err))
	}

	// Get image digest for return value
	imgDigest, err := img.Digest()
	if err != nil {
		return nil, fmt.Errorf("get image digest: %w", err)
	}

	// Pre-create target directory
	if err := os.MkdirAll(targetDir, 0755); err != nil {
		return nil, fmt.Errorf("create target dir: %w", err)
	}

	// Get layers in order
	layers, err := img.Layers()
	if err != nil {
		return nil, fmt.Errorf("get layers: %w", err)
	}

	// Extract each layer in order, applying whiteouts between layers
	for i, layer := range layers {
		mediaType, err := layer.MediaType()
		if err != nil {
			return nil, fmt.Errorf("get layer %d mediatype: %w", i, err)
		}

		// Get uncompressed reader - go-containerregistry handles decompression
		// automatically based on the media type (gzip, zstd, etc.)
		reader, err := layer.Uncompressed()
		if err != nil {
			return nil, fmt.Errorf("get layer %d reader: %w", i, err)
		}

		// Extract layer using tar
		if err := extractTarStream(ctx, reader, targetDir); err != nil {
			reader.Close()
			return nil, fmt.Errorf("extract layer %d (%s): %w", i, mediaType, err)
		}
		reader.Close()

		// Process whiteouts after each layer
		if err := processWhiteouts(targetDir); err != nil {
			return nil, fmt.Errorf("process whiteouts for layer %d: %w", i, err)
		}
	}

	// Extract metadata from image config
	meta, err := extractMetadataFromImage(img)
	if err != nil {
		return nil, fmt.Errorf("extract metadata: %w", err)
	}

	return &pullResult{
		Metadata: meta,
		Digest:   imgDigest.String(),
	}, nil
}

// streamingUnpackFromLayout extracts layers from the local OCI cache to target directory
// without using umoci. This is faster than pullAndExport for local registry images because:
// 1. No network auth required (reads directly from disk)
// 2. Direct tar extraction is 1.6-2.8x faster than umoci
//
// The flow is: OCI Cache Blob -> go-containerregistry -> tar extraction -> rootfs/
// vs pullAndExport: OCI Cache Blob -> umoci -> rootfs/
func (c *ociClient) streamingUnpackFromLayout(ctx context.Context, layoutTag string, targetDir string) (*pullResult, error) {
	// Open OCI layout from local cache
	path, err := layout.FromPath(c.cacheDir)
	if err != nil {
		return nil, fmt.Errorf("open oci layout: %w", err)
	}

	// Get the image by annotation tag from the layout
	img, err := imageByAnnotation(path, layoutTag)
	if err != nil {
		return nil, fmt.Errorf("find image by tag %s: %w", layoutTag, err)
	}

	// Get image digest for return value
	imgDigest, err := img.Digest()
	if err != nil {
		return nil, fmt.Errorf("get image digest: %w", err)
	}

	// Pre-create target directory
	if err := os.MkdirAll(targetDir, 0755); err != nil {
		return nil, fmt.Errorf("create target dir: %w", err)
	}

	// Get layers in order
	layers, err := img.Layers()
	if err != nil {
		return nil, fmt.Errorf("get layers: %w", err)
	}

	// Extract each layer in order, applying whiteouts between layers
	for i, layer := range layers {
		mediaType, err := layer.MediaType()
		if err != nil {
			return nil, fmt.Errorf("get layer %d mediatype: %w", i, err)
		}

		// Get uncompressed reader - go-containerregistry handles decompression
		// automatically based on the media type (gzip, zstd, etc.)
		reader, err := layer.Uncompressed()
		if err != nil {
			return nil, fmt.Errorf("get layer %d reader: %w", i, err)
		}

		// Extract layer using tar
		if err := extractTarStream(ctx, reader, targetDir); err != nil {
			reader.Close()
			return nil, fmt.Errorf("extract layer %d (%s): %w", i, mediaType, err)
		}
		reader.Close()

		// Process whiteouts after each layer
		if err := processWhiteouts(targetDir); err != nil {
			return nil, fmt.Errorf("process whiteouts for layer %d: %w", i, err)
		}
	}

	// Extract metadata from image config
	meta, err := extractMetadataFromImage(img)
	if err != nil {
		return nil, fmt.Errorf("extract metadata: %w", err)
	}

	return &pullResult{
		Metadata: meta,
		Digest:   imgDigest.String(),
	}, nil
}

// extractTarStream extracts a tar stream to the target directory using the tar command.
// This is more reliable than Go's archive/tar for handling all edge cases
// (special files, permissions, extended attributes, etc.)
func extractTarStream(ctx context.Context, reader io.Reader, targetDir string) error {
	// Use tar command for extraction - handles all edge cases properly
	cmd := exec.CommandContext(ctx, "tar", "-xf", "-", "-C", targetDir, "--no-same-owner")
	cmd.Stdin = reader
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("tar extraction failed: %w", err)
	}
	return nil
}

// processWhiteouts handles OCI whiteout files in the target directory.
// Whiteouts are special marker files that indicate deletion in layered filesystems:
// - .wh.<filename>: Delete the file/directory named <filename>
// - .wh..wh..opq: Delete all siblings (opaque directory marker)
//
// This function walks the directory, collects whiteouts, then processes them.
func processWhiteouts(targetDir string) error {
	// Collect whiteouts first, then process (to avoid modifying while walking)
	type whiteout struct {
		path     string // Path to the whiteout file
		isOpaque bool   // True if this is an opaque whiteout
	}
	var whiteouts []whiteout

	err := filepath.Walk(targetDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			// Skip files we can't access
			return nil
		}

		name := info.Name()

		// Check for opaque whiteout (.wh..wh..opq)
		if name == ".wh..wh..opq" {
			whiteouts = append(whiteouts, whiteout{path: path, isOpaque: true})
			return nil
		}

		// Check for regular whiteout (.wh.<filename>)
		if strings.HasPrefix(name, ".wh.") {
			whiteouts = append(whiteouts, whiteout{path: path, isOpaque: false})
			return nil
		}

		return nil
	})
	if err != nil {
		return fmt.Errorf("walk directory: %w", err)
	}

	// Process whiteouts
	for _, wh := range whiteouts {
		if wh.isOpaque {
			// Opaque whiteout: remove all siblings in the parent directory
			// that existed BEFORE this layer (we can't tell, so we remove all)
			parentDir := filepath.Dir(wh.path)
			entries, err := os.ReadDir(parentDir)
			if err != nil {
				return fmt.Errorf("read dir for opaque whiteout %s: %w", parentDir, err)
			}

			for _, entry := range entries {
				// Skip the opaque marker itself and other whiteouts
				if entry.Name() == ".wh..wh..opq" || strings.HasPrefix(entry.Name(), ".wh.") {
					continue
				}
				entryPath := filepath.Join(parentDir, entry.Name())
				if err := os.RemoveAll(entryPath); err != nil {
					return fmt.Errorf("remove %s for opaque whiteout: %w", entryPath, err)
				}
			}

			// Remove the opaque marker itself
			if err := os.Remove(wh.path); err != nil {
				return fmt.Errorf("remove opaque marker %s: %w", wh.path, err)
			}
		} else {
			// Regular whiteout: remove the target file
			// .wh.filename -> delete filename
			whiteoutFile := filepath.Base(wh.path)
			targetName := strings.TrimPrefix(whiteoutFile, ".wh.")
			targetPath := filepath.Join(filepath.Dir(wh.path), targetName)

			// Remove the target (may not exist if it was never created)
			if err := os.RemoveAll(targetPath); err != nil && !os.IsNotExist(err) {
				return fmt.Errorf("remove %s for whiteout: %w", targetPath, err)
			}

			// Remove the whiteout marker itself
			if err := os.Remove(wh.path); err != nil {
				return fmt.Errorf("remove whiteout marker %s: %w", wh.path, err)
			}
		}
	}

	return nil
}

// extractMetadataFromImage extracts container metadata directly from a go-containerregistry
// image object. This is used by streamingUnpack to get metadata without needing
// the OCI cache.
func extractMetadataFromImage(img gcr.Image) (*containerMetadata, error) {
	configFile, err := img.ConfigFile()
	if err != nil {
		return nil, fmt.Errorf("get config file: %w", err)
	}

	meta := &containerMetadata{
		Entrypoint: configFile.Config.Entrypoint,
		Cmd:        configFile.Config.Cmd,
		Env:        make(map[string]string),
		WorkingDir: configFile.Config.WorkingDir,
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

	return meta, nil
}
