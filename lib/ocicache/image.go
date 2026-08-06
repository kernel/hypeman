// Package ocicache reads OCI images from hypeman's shared OCI blob cache.
//
// The cache is a content-addressable blob store under <data-dir>/system/oci-cache
// (see lib/paths). Images pushed to the embedded registry or pulled from remote
// registries are stored there as manifest, config, and layer blobs. This package
// reconstructs a go-containerregistry v1.Image from those blobs so callers can
// inspect or re-push cached images without a daemon.
package ocicache

import (
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/types"
	"github.com/kernel/hypeman/lib/paths"
)

// ErrNotFound means no manifest blob exists in the cache for the requested digest.
var ErrNotFound = errors.New("image not found in OCI cache")

// ErrInvalidDigest means a digest is not a well-formed sha256 hex string and
// cannot be used as a cache path component.
var ErrInvalidDigest = errors.New("invalid image digest")

// blobHex validates a digest (optionally "sha256:"-prefixed) and returns its
// hex form. The hex form is joined onto the blob directory path
// (lib/paths.OCICacheBlob), so anything other than exactly 64 lowercase hex
// chars would allow path traversal outside the cache.
func blobHex(digest string) (string, error) {
	hexDigest := strings.TrimPrefix(digest, "sha256:")
	if len(hexDigest) != 64 {
		return "", fmt.Errorf("%w: %s", ErrInvalidDigest, digest)
	}
	for i := 0; i < len(hexDigest); i++ {
		c := hexDigest[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return "", fmt.Errorf("%w: %s", ErrInvalidDigest, digest)
		}
	}
	return hexDigest, nil
}

// ImageFromCache creates a v1.Image that reads from the OCI blob cache.
// Docker v2 manifests are transparently converted to OCI format, matching the
// conversion applied when images are appended to the OCI layout.
func ImageFromCache(p *paths.Paths, digest string) (v1.Image, error) {
	digestHex, err := blobHex(digest)
	if err != nil {
		return nil, err
	}
	manifestData, err := os.ReadFile(p.OCICacheBlob(digestHex))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("%w: %s", ErrNotFound, digest)
		}
		return nil, fmt.Errorf("read manifest: %w", err)
	}

	return &cacheImage{
		paths:        p,
		manifestData: manifestData,
		digest:       digest,
	}, nil
}

// cacheImage implements v1.Image by reading from the blob cache.
// It transparently converts Docker v2 manifests to OCI format.
type cacheImage struct {
	paths        *paths.Paths
	manifestData []byte
	digest       string

	manifestOnce sync.Once
	manifest     *v1.Manifest
	manifestErr  error
}

// Layers returns cacheLayer instances for each layer in the manifest.
func (img *cacheImage) Layers() ([]v1.Layer, error) {
	manifest, err := img.parsedManifest()
	if err != nil {
		return nil, err
	}

	var layers []v1.Layer
	for _, layerDesc := range manifest.Layers {
		layer := &cacheLayer{
			paths:     img.paths,
			digest:    layerDesc.Digest.String(),
			size:      layerDesc.Size,
			mediaType: string(layerDesc.MediaType),
		}
		layers = append(layers, layer)
	}
	return layers, nil
}

// MediaType returns OCI manifest type, converting from Docker v2 if needed.
func (img *cacheImage) MediaType() (types.MediaType, error) {
	manifest, err := img.parsedManifest()
	if err != nil {
		return "", err
	}
	if isOCIMediaType(string(manifest.MediaType)) {
		return manifest.MediaType, nil
	}
	return types.OCIManifestSchema1, nil
}

// isOCIMediaType returns true if the media type is an OCI manifest type
func isOCIMediaType(mediaType string) bool {
	return mediaType == string(types.OCIManifestSchema1)
}

func (img *cacheImage) Size() (int64, error) {
	raw, err := img.RawManifest()
	if err != nil {
		return 0, err
	}
	return int64(len(raw)), nil
}

func (img *cacheImage) ConfigName() (v1.Hash, error) {
	manifest, err := img.parsedManifest()
	if err != nil {
		return v1.Hash{}, err
	}
	return manifest.Config.Digest, nil
}

func (img *cacheImage) ConfigFile() (*v1.ConfigFile, error) {
	manifest, err := img.parsedManifest()
	if err != nil {
		return nil, err
	}

	configPath := img.paths.OCICacheBlob(manifest.Config.Digest.Hex)
	configData, err := os.ReadFile(configPath)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}

	var config v1.ConfigFile
	if err := json.Unmarshal(configData, &config); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	return &config, nil
}

func (img *cacheImage) RawConfigFile() ([]byte, error) {
	manifest, err := img.parsedManifest()
	if err != nil {
		return nil, err
	}

	configPath := img.paths.OCICacheBlob(manifest.Config.Digest.Hex)
	return os.ReadFile(configPath)
}

// Digest returns the manifest digest. For Docker v2, returns the digest of the
// converted OCI manifest (which differs from the original Docker v2 digest).
func (img *cacheImage) Digest() (v1.Hash, error) {
	manifest, err := img.parsedManifest()
	if err != nil {
		return v1.Hash{}, err
	}
	if isOCIMediaType(string(manifest.MediaType)) {
		return v1.NewHash(img.digest)
	}
	rawManifest, err := img.RawManifest()
	if err != nil {
		return v1.Hash{}, err
	}
	sum := sha256.Sum256(rawManifest)
	return v1.Hash{
		Algorithm: "sha256",
		Hex:       hex.EncodeToString(sum[:]),
	}, nil
}

// Manifest returns the parsed manifest with Docker v2 media types converted to
// OCI. OCI manifests are returned as-is with all fields preserved. Docker v2
// manifests are copied and converted, preserving annotations, subject, and
// descriptor urls/platform so the converted form is not lossy.
func (img *cacheImage) Manifest() (*v1.Manifest, error) {
	manifest, err := img.parsedManifest()
	if err != nil {
		return nil, err
	}
	if isOCIMediaType(string(manifest.MediaType)) {
		return manifest, nil
	}

	// Copy so the cached parse is never mutated (parsedManifest caches the
	// result); fresh slices for the fields we rewrite.
	conv := *manifest
	conv.MediaType = types.OCIManifestSchema1
	conv.Config.MediaType = convertToOCIMediaType(string(manifest.Config.MediaType))
	conv.Layers = make([]v1.Descriptor, len(manifest.Layers))
	for i, layer := range manifest.Layers {
		conv.Layers[i] = layer
		conv.Layers[i].MediaType = convertToOCIMediaType(string(layer.MediaType))
	}
	return &conv, nil
}

// convertToOCIMediaType converts Docker v2 media types to OCI equivalents
func convertToOCIMediaType(mediaType string) types.MediaType {
	switch mediaType {
	case "application/vnd.docker.distribution.manifest.v2+json":
		return types.OCIManifestSchema1
	case "application/vnd.docker.container.image.v1+json":
		return types.OCIConfigJSON
	case "application/vnd.docker.image.rootfs.diff.tar.gzip":
		return types.OCILayer
	case "application/vnd.docker.image.rootfs.diff.tar":
		return types.OCIUncompressedLayer
	default:
		// If already OCI or unknown, return as-is
		return types.MediaType(mediaType)
	}
}

// RawManifest returns the manifest JSON. For OCI, returns original bytes to preserve
// digest. For Docker v2, returns the converted OCI manifest JSON.
func (img *cacheImage) RawManifest() ([]byte, error) {
	manifest, err := img.parsedManifest()
	if err != nil {
		return nil, err
	}
	if isOCIMediaType(string(manifest.MediaType)) {
		return img.manifestData, nil
	}
	v1Manifest, err := img.Manifest()
	if err != nil {
		return nil, err
	}
	return json.Marshal(v1Manifest)
}

func (img *cacheImage) LayerByDigest(hash v1.Hash) (v1.Layer, error) {
	manifest, err := img.parsedManifest()
	if err != nil {
		return nil, err
	}

	for _, layer := range manifest.Layers {
		if layer.Digest == hash {
			return &cacheLayer{
				paths:     img.paths,
				digest:    layer.Digest.String(),
				size:      layer.Size,
				mediaType: string(layer.MediaType),
			}, nil
		}
	}
	return nil, fmt.Errorf("%w: %s", ErrNotFound, hash.String())
}

func (img *cacheImage) LayerByDiffID(hash v1.Hash) (v1.Layer, error) {
	return nil, fmt.Errorf("layerByDiffID not implemented")
}

// parsedManifest parses manifestData once and caches the result, since the
// bytes come from a content-addressed blob store and are immutable.
func (img *cacheImage) parsedManifest() (*v1.Manifest, error) {
	img.manifestOnce.Do(func() {
		var manifest v1.Manifest
		if err := json.Unmarshal(img.manifestData, &manifest); err != nil {
			img.manifestErr = fmt.Errorf("parse manifest: %w", err)
			return
		}
		img.manifest = &manifest
	})
	return img.manifest, img.manifestErr
}

// cacheLayer implements v1.Layer by reading blobs from the filesystem.
// Layer content is served directly; media types are converted to OCI format.
type cacheLayer struct {
	paths     *paths.Paths
	digest    string
	size      int64
	mediaType string
}

// Digest returns the layer's content hash.
func (l *cacheLayer) Digest() (v1.Hash, error) {
	return v1.NewHash(l.digest)
}

// Compressed returns a reader for the compressed layer blob from disk.
func (l *cacheLayer) Compressed() (io.ReadCloser, error) {
	digestHex, err := blobHex(l.digest)
	if err != nil {
		return nil, err
	}
	blobPath := l.paths.OCICacheBlob(digestHex)
	return os.Open(blobPath)
}

// DiffID returns an error: computing the true DiffID requires decompressing
// the layer, which is expensive and not always needed. Callers that need it
// should read Uncompressed() and hash the result.
func (l *cacheLayer) DiffID() (v1.Hash, error) {
	return v1.Hash{}, fmt.Errorf("diffID for cached layer %s requires decompressing; read Uncompressed() and hash", l.digest)
}

// Uncompressed returns a reader for the uncompressed layer content. Gzip
// layers are decompressed on the fly; already-uncompressed (tar) layers are
// served as stored. Other media types are unsupported because the cache
// stores blobs as-is.
func (l *cacheLayer) Uncompressed() (io.ReadCloser, error) {
	rc, err := l.Compressed()
	if err != nil {
		return nil, err
	}
	switch l.mediaType {
	case string(types.OCILayer), string(types.DockerLayer):
		gz, err := gzip.NewReader(rc)
		if err != nil {
			rc.Close()
			return nil, fmt.Errorf("decompress layer %s: %w", l.digest, err)
		}
		return &closerChain{Reader: gz, closers: []io.Closer{gz, rc}}, nil
	case string(types.OCIUncompressedLayer), string(types.DockerUncompressedLayer):
		return rc, nil
	default:
		rc.Close()
		return nil, fmt.Errorf("unsupported layer media type for decompression: %s", l.mediaType)
	}
}

// closerChain closes all closers in order, returning the first error.
type closerChain struct {
	io.Reader
	closers []io.Closer
}

func (c *closerChain) Close() error {
	var firstErr error
	for _, closer := range c.closers {
		if err := closer.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// Size returns the compressed size of the layer in bytes.
func (l *cacheLayer) Size() (int64, error) {
	return l.size, nil
}

// MediaType returns the layer's media type, converting Docker v2 types to OCI.
func (l *cacheLayer) MediaType() (types.MediaType, error) {
	return convertToOCIMediaType(l.mediaType), nil
}
