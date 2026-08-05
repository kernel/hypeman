// Package ocicache reads OCI images from hypeman's shared OCI blob cache.
//
// The cache is a content-addressable blob store under <data-dir>/system/oci-cache
// (see lib/paths). Images pushed to the embedded registry or pulled from remote
// registries are stored there as manifest, config, and layer blobs. This package
// reconstructs a go-containerregistry v1.Image from those blobs so callers can
// inspect or re-push cached images without a daemon.
package ocicache

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/types"
	"github.com/kernel/hypeman/lib/paths"
)

// ErrNotFound means no manifest blob exists in the cache for the requested digest.
var ErrNotFound = errors.New("image not found in OCI cache")

// ImageFromCache creates a v1.Image that reads from the OCI blob cache.
// Docker v2 manifests are transparently converted to OCI format, matching the
// conversion applied when images are appended to the OCI layout.
func ImageFromCache(p *paths.Paths, digest string) (v1.Image, error) {
	digestHex := strings.TrimPrefix(digest, "sha256:")
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
}

// Layers returns cacheLayer instances for each layer in the manifest.
func (img *cacheImage) Layers() ([]v1.Layer, error) {
	manifest, err := img.parseManifest()
	if err != nil {
		return nil, err
	}

	var layers []v1.Layer
	for _, layerDesc := range manifest.Layers {
		layer := &cacheLayer{
			paths:     img.paths,
			digest:    layerDesc.Digest,
			size:      layerDesc.Size,
			mediaType: layerDesc.MediaType,
		}
		layers = append(layers, layer)
	}
	return layers, nil
}

// MediaType returns OCI manifest type, converting from Docker v2 if needed.
func (img *cacheImage) MediaType() (types.MediaType, error) {
	manifest, err := img.parseManifest()
	if err != nil {
		return "", err
	}
	if isOCIMediaType(manifest.MediaType) {
		return types.MediaType(manifest.MediaType), nil
	}
	return types.OCIManifestSchema1, nil
}

// isOCIMediaType returns true if the media type is an OCI manifest type
func isOCIMediaType(mediaType string) bool {
	return mediaType == string(types.OCIManifestSchema1) ||
		mediaType == "application/vnd.oci.image.manifest.v1+json"
}

func (img *cacheImage) Size() (int64, error) {
	manifest, err := img.parseManifest()
	if err != nil {
		return 0, err
	}
	if isOCIMediaType(manifest.MediaType) {
		return int64(len(img.manifestData)), nil
	}
	rawManifest, err := img.RawManifest()
	if err != nil {
		return 0, err
	}
	return int64(len(rawManifest)), nil
}

func (img *cacheImage) ConfigName() (v1.Hash, error) {
	manifest, err := img.parseManifest()
	if err != nil {
		return v1.Hash{}, err
	}
	h, err := v1.NewHash(manifest.Config.Digest)
	if err != nil {
		return v1.Hash{}, err
	}
	return h, nil
}

func (img *cacheImage) ConfigFile() (*v1.ConfigFile, error) {
	manifest, err := img.parseManifest()
	if err != nil {
		return nil, err
	}

	digestHex := strings.TrimPrefix(manifest.Config.Digest, "sha256:")
	configPath := img.paths.OCICacheBlob(digestHex)
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
	manifest, err := img.parseManifest()
	if err != nil {
		return nil, err
	}

	digestHex := strings.TrimPrefix(manifest.Config.Digest, "sha256:")
	configPath := img.paths.OCICacheBlob(digestHex)
	return os.ReadFile(configPath)
}

// Digest returns the manifest digest. For Docker v2, returns the digest of the
// converted OCI manifest (which differs from the original Docker v2 digest).
func (img *cacheImage) Digest() (v1.Hash, error) {
	manifest, err := img.parseManifest()
	if err != nil {
		return v1.Hash{}, err
	}
	if isOCIMediaType(manifest.MediaType) {
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

// Manifest returns the parsed manifest with Docker v2 media types converted to OCI.
func (img *cacheImage) Manifest() (*v1.Manifest, error) {
	manifest, err := img.parseManifest()
	if err != nil {
		return nil, err
	}

	targetMediaType := types.OCIManifestSchema1
	if isOCIMediaType(manifest.MediaType) {
		targetMediaType = types.MediaType(manifest.MediaType)
	}

	v1Manifest := &v1.Manifest{
		SchemaVersion: int64(manifest.SchemaVersion),
		MediaType:     targetMediaType,
		Config: v1.Descriptor{
			MediaType: convertToOCIMediaType(manifest.Config.MediaType),
			Size:      manifest.Config.Size,
		},
	}

	configHash, err := v1.NewHash(manifest.Config.Digest)
	if err != nil {
		return nil, err
	}
	v1Manifest.Config.Digest = configHash

	for _, layer := range manifest.Layers {
		layerHash, err := v1.NewHash(layer.Digest)
		if err != nil {
			return nil, err
		}
		v1Manifest.Layers = append(v1Manifest.Layers, v1.Descriptor{
			MediaType: convertToOCIMediaType(layer.MediaType),
			Size:      layer.Size,
			Digest:    layerHash,
		})
	}

	return v1Manifest, nil
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
	manifest, err := img.parseManifest()
	if err != nil {
		return nil, err
	}
	if isOCIMediaType(manifest.MediaType) {
		return img.manifestData, nil
	}
	v1Manifest, err := img.Manifest()
	if err != nil {
		return nil, err
	}
	return json.Marshal(v1Manifest)
}

func (img *cacheImage) LayerByDigest(hash v1.Hash) (v1.Layer, error) {
	manifest, err := img.parseManifest()
	if err != nil {
		return nil, err
	}

	for _, layer := range manifest.Layers {
		if layer.Digest == hash.String() {
			return &cacheLayer{
				paths:     img.paths,
				digest:    layer.Digest,
				size:      layer.Size,
				mediaType: layer.MediaType,
			}, nil
		}
	}
	return nil, fmt.Errorf("layer not found: %s", hash.String())
}

func (img *cacheImage) LayerByDiffID(hash v1.Hash) (v1.Layer, error) {
	return nil, fmt.Errorf("LayerByDiffID not implemented")
}

// internal manifest structure for parsing
type internalManifest struct {
	SchemaVersion int    `json:"schemaVersion"`
	MediaType     string `json:"mediaType"`
	Config        struct {
		MediaType string `json:"mediaType"`
		Size      int64  `json:"size"`
		Digest    string `json:"digest"`
	} `json:"config"`
	Layers []struct {
		MediaType string `json:"mediaType"`
		Size      int64  `json:"size"`
		Digest    string `json:"digest"`
	} `json:"layers"`
}

func (img *cacheImage) parseManifest() (*internalManifest, error) {
	var manifest internalManifest
	if err := json.Unmarshal(img.manifestData, &manifest); err != nil {
		return nil, fmt.Errorf("parse manifest: %w", err)
	}
	return &manifest, nil
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

// DiffID returns an empty hash. Computing the actual DiffID requires decompressing
// the layer which is expensive; callers that need DiffID should compute it themselves.
func (l *cacheLayer) DiffID() (v1.Hash, error) {
	return v1.Hash{}, nil
}

// Compressed returns a reader for the compressed layer blob from disk.
func (l *cacheLayer) Compressed() (io.ReadCloser, error) {
	digestHex := strings.TrimPrefix(l.digest, "sha256:")
	blobPath := l.paths.OCICacheBlob(digestHex)
	return os.Open(blobPath)
}

// Uncompressed returns a reader for the layer content. Since layers are stored
// compressed, this returns the compressed stream and relies on the caller
// (go-containerregistry) to handle decompression based on MediaType.
func (l *cacheLayer) Uncompressed() (io.ReadCloser, error) {
	return l.Compressed()
}

// Size returns the compressed size of the layer in bytes.
func (l *cacheLayer) Size() (int64, error) {
	return l.size, nil
}

// MediaType returns the layer's media type, converting Docker v2 types to OCI.
func (l *cacheLayer) MediaType() (types.MediaType, error) {
	return convertToOCIMediaType(l.mediaType), nil
}
