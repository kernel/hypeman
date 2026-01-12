// Package registry implements an OCI Distribution Spec registry that accepts pushed images
// and triggers conversion to hypeman's disk format.
package registry

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strings"

	"github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/layout"
	"github.com/google/go-containerregistry/pkg/v1/types"
	"github.com/kernel/hypeman/lib/images"
	"github.com/kernel/hypeman/lib/paths"
)

// Registry provides an OCI Distribution Spec compliant registry that stores pushed images
// in hypeman's OCI cache and triggers conversion to ext4 disk format.
type Registry struct {
	paths        *paths.Paths
	imageManager images.Manager
	blobStore    *BlobStore
	handler      http.Handler
}

// manifestPutPattern matches PUT requests to /v2/{name}/manifests/{reference}
var manifestPutPattern = regexp.MustCompile(`^/v2/(.+)/manifests/(.+)$`)

// New creates a new Registry that stores blobs in the OCI cache directory
// and triggers image conversion when manifests are pushed.
func New(p *paths.Paths, imgManager images.Manager) (*Registry, error) {
	blobStore, err := NewBlobStore(p)
	if err != nil {
		return nil, err
	}

	// Create registry with custom blob handler
	regHandler := registry.New(
		registry.WithBlobHandler(blobStore),
	)

	r := &Registry{
		paths:        p,
		imageManager: imgManager,
		blobStore:    blobStore,
		handler:      regHandler,
	}

	return r, nil
}

// Handler returns the http.Handler for the registry endpoints.
// This wraps the underlying registry to intercept manifest PUTs and trigger conversion.
func (r *Registry) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		// Intercept manifest PUT requests to store in blob store and trigger conversion
		if req.Method == http.MethodPut {
			matches := manifestPutPattern.FindStringSubmatch(req.URL.Path)
			if matches != nil {
				pathRepo := matches[1]
				reference := matches[2]

				// Include the host to form the full repository path
				// This preserves the registry host (e.g., "10.102.0.1:8083/builds/xxx")
				// instead of normalizing to docker.io
				fullRepo := pathRepo
				if req.Host != "" {
					fullRepo = req.Host + "/" + pathRepo
				}

				body, err := io.ReadAll(req.Body)
				req.Body.Close()
				if err != nil {
					http.Error(w, "failed to read body", http.StatusInternalServerError)
					return
				}

				digest := computeDigest(body)

				// Verify digest if reference is a digest
				if strings.HasPrefix(reference, "sha256:") && reference != digest {
					http.Error(w, fmt.Sprintf("digest mismatch: expected %s, got %s", reference, digest), http.StatusBadRequest)
					return
				}

				if err := r.storeManifestBlob(digest, body); err != nil {
					fmt.Fprintf(os.Stderr, "Warning: failed to store manifest blob: %v\n", err)
				}

				req.Body = io.NopCloser(bytes.NewReader(body))
				wrapper := &responseWrapper{ResponseWriter: w}
				r.handler.ServeHTTP(wrapper, req)

				if wrapper.statusCode == http.StatusCreated {
					go r.triggerConversion(fullRepo, reference, digest)
				}
				return
			}
		}

		r.handler.ServeHTTP(w, req)
	})
}

// storeManifestBlob stores a manifest in the blob store by its digest.
func (r *Registry) storeManifestBlob(digest string, data []byte) error {
	digestHex := strings.TrimPrefix(digest, "sha256:")
	blobPath := r.paths.OCICacheBlob(digestHex)

	// Verify digest matches
	actualDigest := computeDigest(data)
	if actualDigest != digest {
		return fmt.Errorf("digest mismatch: expected %s, got %s", digest, actualDigest)
	}

	return os.WriteFile(blobPath, data, 0644)
}

// responseWrapper captures the status code from the response
type responseWrapper struct {
	http.ResponseWriter
	statusCode int
}

func (w *responseWrapper) WriteHeader(code int) {
	w.statusCode = code
	w.ResponseWriter.WriteHeader(code)
}

// triggerConversion queues the image for conversion to ext4 disk format.
func (r *Registry) triggerConversion(repo, reference, dockerDigest string) {
	imageRef := repo + ":" + reference
	if strings.HasPrefix(reference, "sha256:") {
		imageRef = repo + "@" + reference
	}

	ociDigest, err := r.addToOCILayout(dockerDigest)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to add image to OCI layout for %s: %v\n", imageRef, err)
		return
	}

	_, err = r.imageManager.ImportLocalImage(context.Background(), repo, reference, ociDigest)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to queue image conversion for %s: %v\n", imageRef, err)
	}
}

// addToOCILayout adds the image to the OCI layout, converting Docker v2 to OCI if needed.
func (r *Registry) addToOCILayout(inputDigest string) (string, error) {
	cacheDir := r.paths.SystemOCICache()
	path, err := layout.FromPath(cacheDir)
	if err != nil {
		path, err = layout.Write(cacheDir, empty.Index)
		if err != nil {
			return "", fmt.Errorf("create oci layout: %w", err)
		}
	}

	img, err := r.imageFromBlobStore(inputDigest)
	if err != nil {
		return "", fmt.Errorf("create image from blob store: %w", err)
	}

	digestHash, err := img.Digest()
	if err != nil {
		return "", fmt.Errorf("compute digest: %w", err)
	}
	digest := digestHash.String()
	digestHex := digestHash.Hex

	err = path.AppendImage(img, layout.WithAnnotations(map[string]string{
		"org.opencontainers.image.ref.name": digestHex,
	}))
	if err != nil {
		return "", fmt.Errorf("append image to layout: %w", err)
	}

	return digest, nil
}

// imageFromBlobStore creates a v1.Image that reads from our blob store.
func (r *Registry) imageFromBlobStore(digest string) (v1.Image, error) {
	digestHex := strings.TrimPrefix(digest, "sha256:")
	manifestPath := r.paths.OCICacheBlob(digestHex)

	manifestData, err := os.ReadFile(manifestPath)
	if err != nil {
		return nil, fmt.Errorf("read manifest: %w", err)
	}

	return &blobStoreImage{
		paths:        r.paths,
		manifestData: manifestData,
		digest:       digest,
	}, nil
}

// blobStoreImage implements v1.Image by reading from the blob store.
// It transparently converts Docker v2 manifests to OCI format.
type blobStoreImage struct {
	paths        *paths.Paths
	manifestData []byte
	digest       string
}

// Layers returns wrapped blobStoreLayer instances for each layer in the manifest.
func (img *blobStoreImage) Layers() ([]v1.Layer, error) {
	manifest, err := img.parseManifest()
	if err != nil {
		return nil, err
	}

	var layers []v1.Layer
	for _, layerDesc := range manifest.Layers {
		layer := &blobStoreLayer{
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
func (img *blobStoreImage) MediaType() (types.MediaType, error) {
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

func (img *blobStoreImage) Size() (int64, error) {
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

func (img *blobStoreImage) ConfigName() (v1.Hash, error) {
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

func (img *blobStoreImage) ConfigFile() (*v1.ConfigFile, error) {
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

func (img *blobStoreImage) RawConfigFile() ([]byte, error) {
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
func (img *blobStoreImage) Digest() (v1.Hash, error) {
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
func (img *blobStoreImage) Manifest() (*v1.Manifest, error) {
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
func (img *blobStoreImage) RawManifest() ([]byte, error) {
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

func (img *blobStoreImage) LayerByDigest(hash v1.Hash) (v1.Layer, error) {
	manifest, err := img.parseManifest()
	if err != nil {
		return nil, err
	}

	for _, layer := range manifest.Layers {
		if layer.Digest == hash.String() {
			return &blobStoreLayer{
				paths:     img.paths,
				digest:    layer.Digest,
				size:      layer.Size,
				mediaType: layer.MediaType,
			}, nil
		}
	}
	return nil, fmt.Errorf("layer not found: %s", hash.String())
}

func (img *blobStoreImage) LayerByDiffID(hash v1.Hash) (v1.Layer, error) {
	return nil, fmt.Errorf("LayerByDiffID not implemented")
}

// Internal manifest structure for parsing
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

func (img *blobStoreImage) parseManifest() (*internalManifest, error) {
	var manifest internalManifest
	if err := json.Unmarshal(img.manifestData, &manifest); err != nil {
		return nil, fmt.Errorf("parse manifest: %w", err)
	}
	return &manifest, nil
}

// blobStoreLayer implements v1.Layer by reading blobs from the filesystem.
// Layer content is served directly; media types are converted to OCI format.
type blobStoreLayer struct {
	paths     *paths.Paths
	digest    string
	size      int64
	mediaType string
}

// Digest returns the layer's content hash.
func (l *blobStoreLayer) Digest() (v1.Hash, error) {
	return v1.NewHash(l.digest)
}

// DiffID returns an empty hash. Computing the actual DiffID requires decompressing
// the layer which is expensive; callers that need DiffID should compute it themselves.
func (l *blobStoreLayer) DiffID() (v1.Hash, error) {
	return v1.Hash{}, nil
}

// Compressed returns a reader for the compressed layer blob from disk.
func (l *blobStoreLayer) Compressed() (io.ReadCloser, error) {
	digestHex := strings.TrimPrefix(l.digest, "sha256:")
	blobPath := l.paths.OCICacheBlob(digestHex)
	return os.Open(blobPath)
}

// Uncompressed returns a reader for the layer content. Since layers are stored
// compressed, this returns the compressed stream and relies on the caller
// (go-containerregistry) to handle decompression based on MediaType.
func (l *blobStoreLayer) Uncompressed() (io.ReadCloser, error) {
	return l.Compressed()
}

// Size returns the compressed size of the layer in bytes.
func (l *blobStoreLayer) Size() (int64, error) {
	return l.size, nil
}

// MediaType returns the layer's media type, converting Docker v2 types to OCI.
func (l *blobStoreLayer) MediaType() (types.MediaType, error) {
	return convertToOCIMediaType(l.mediaType), nil
}

// computeDigest calculates SHA256 hash of data
func computeDigest(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}
