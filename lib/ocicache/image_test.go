package ocicache

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
	"testing"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/types"
	"github.com/kernel/hypeman/lib/ocicache/testutil"
	"github.com/kernel/hypeman/lib/paths"
)

// writeCacheImage stores an image's blobs in a temp OCI cache and returns the
// manifest digest.
func writeCacheImage(t *testing.T, p *paths.Paths, img v1.Image) string {
	t.Helper()
	digest, err := testutil.WriteImage(p, img)
	if err != nil {
		t.Fatalf("write cache: %v", err)
	}
	return digest
}

func tempPaths(t *testing.T) *paths.Paths {
	t.Helper()
	return paths.New(t.TempDir())
}

func TestImageFromCacheRoundTrip(t *testing.T) {
	p := tempPaths(t)
	randomImg, err := random.Image(256, 2)
	if err != nil {
		t.Fatalf("random image: %v", err)
	}
	// Convert to OCI media types so the cached manifest round-trips
	// byte-for-byte (Docker v2 conversion is covered separately).
	img := mutate.MediaType(randomImg, types.OCIManifestSchema1)
	digest := writeCacheImage(t, p, img)

	cached, err := ImageFromCache(p, digest)
	if err != nil {
		t.Fatalf("ImageFromCache: %v", err)
	}

	gotDigest, err := cached.Digest()
	if err != nil {
		t.Fatalf("digest: %v", err)
	}
	if gotDigest.String() != digest {
		t.Errorf("digest = %s, want %s", gotDigest, digest)
	}

	wantManifest, err := img.RawManifest()
	if err != nil {
		t.Fatalf("original raw manifest: %v", err)
	}
	gotManifest, err := cached.RawManifest()
	if err != nil {
		t.Fatalf("cached raw manifest: %v", err)
	}
	if !bytes.Equal(gotManifest, wantManifest) {
		t.Error("cached manifest bytes differ from original")
	}

	wantConfig, err := img.ConfigFile()
	if err != nil {
		t.Fatalf("original config: %v", err)
	}
	gotConfig, err := cached.ConfigFile()
	if err != nil {
		t.Fatalf("cached config: %v", err)
	}
	if wantConfig.Architecture != gotConfig.Architecture || wantConfig.OS != gotConfig.OS {
		t.Errorf("config mismatch: got %s/%s, want %s/%s",
			gotConfig.OS, gotConfig.Architecture, wantConfig.OS, wantConfig.Architecture)
	}

	wantLayers, err := img.Layers()
	if err != nil {
		t.Fatalf("original layers: %v", err)
	}
	gotLayers, err := cached.Layers()
	if err != nil {
		t.Fatalf("cached layers: %v", err)
	}
	if len(gotLayers) != len(wantLayers) {
		t.Fatalf("layers = %d, want %d", len(gotLayers), len(wantLayers))
	}
	for i := range wantLayers {
		wantHash, err := wantLayers[i].Digest()
		if err != nil {
			t.Fatalf("original layer digest: %v", err)
		}
		gotHash, err := gotLayers[i].Digest()
		if err != nil {
			t.Fatalf("cached layer digest: %v", err)
		}
		if gotHash != wantHash {
			t.Errorf("layer %d digest = %s, want %s", i, gotHash, wantHash)
		}

		rc, err := gotLayers[i].Compressed()
		if err != nil {
			t.Fatalf("cached layer reader: %v", err)
		}
		data, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			t.Fatalf("read cached layer: %v", err)
		}
		sum := sha256.Sum256(data)
		if hex.EncodeToString(sum[:]) != gotHash.Hex {
			t.Errorf("layer %d blob content does not match digest", i)
		}

		// Uncompressed content must match the source layer, and DiffID must
		// refuse to lie rather than returning a zero hash.
		wantUncompressed, err := wantLayers[i].Uncompressed()
		if err != nil {
			t.Fatalf("original layer uncompressed: %v", err)
		}
		wantData, err := io.ReadAll(wantUncompressed)
		wantUncompressed.Close()
		if err != nil {
			t.Fatalf("read original uncompressed layer: %v", err)
		}
		gotUncompressed, err := gotLayers[i].Uncompressed()
		if err != nil {
			t.Fatalf("cached layer uncompressed: %v", err)
		}
		gotData, err := io.ReadAll(gotUncompressed)
		gotUncompressed.Close()
		if err != nil {
			t.Fatalf("read cached uncompressed layer: %v", err)
		}
		if !bytes.Equal(gotData, wantData) {
			t.Errorf("layer %d uncompressed content differs from source", i)
		}
		if _, err := gotLayers[i].DiffID(); err == nil {
			t.Errorf("layer %d DiffID() should error, got nil", i)
		}
	}
}

func TestImageFromCacheNotFound(t *testing.T) {
	p := tempPaths(t)

	_, err := ImageFromCache(p, "sha256:"+hex.EncodeToString(make([]byte, 32)))
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestImageFromCacheRejectsInvalidDigests(t *testing.T) {
	p := tempPaths(t)
	for _, digest := range []string{
		"sha256:../../etc/passwd",
		"sha256:" + strings.Repeat("Z", 64),
		"sha256:deadbeef",
		"not-a-digest",
	} {
		if _, err := ImageFromCache(p, digest); !errors.Is(err, ErrInvalidDigest) {
			t.Errorf("ImageFromCache(%q) err = %v, want ErrInvalidDigest", digest, err)
		}
	}
}

// dockerV2Manifest rewrites an OCI manifest's media types to Docker v2 form.
func dockerV2Manifest(t *testing.T, img v1.Image) []byte {
	t.Helper()

	manifest, err := img.Manifest()
	if err != nil {
		t.Fatalf("manifest: %v", err)
	}

	type descriptor struct {
		MediaType string `json:"mediaType"`
		Size      int64  `json:"size"`
		Digest    string `json:"digest"`
	}
	dockerManifest := struct {
		SchemaVersion int          `json:"schemaVersion"`
		MediaType     string       `json:"mediaType"`
		Config        descriptor   `json:"config"`
		Layers        []descriptor `json:"layers"`
	}{
		SchemaVersion: 2,
		MediaType:     string(types.DockerManifestSchema2),
		Config: descriptor{
			MediaType: string(types.DockerConfigJSON),
			Size:      manifest.Config.Size,
			Digest:    manifest.Config.Digest.String(),
		},
	}
	for _, layer := range manifest.Layers {
		dockerManifest.Layers = append(dockerManifest.Layers, descriptor{
			MediaType: string(types.DockerLayer),
			Size:      layer.Size,
			Digest:    layer.Digest.String(),
		})
	}

	data, err := json.Marshal(dockerManifest)
	if err != nil {
		t.Fatalf("marshal docker manifest: %v", err)
	}
	return data
}

// TestImageFromCacheDockerPreservesForeignLayerURLs pins the non-lossy
// Docker→OCI conversion: descriptor urls (foreign layers) survive
// Manifest()/RawManifest() and the converted digest is the sha256 of the
// converted bytes.
func TestImageFromCacheDockerPreservesForeignLayerURLs(t *testing.T) {
	p := tempPaths(t)
	img, err := random.Image(256, 1)
	if err != nil {
		t.Fatalf("random image: %v", err)
	}
	writeCacheImage(t, p, img)

	manifest, err := img.Manifest()
	if err != nil {
		t.Fatalf("manifest: %v", err)
	}
	type descriptor struct {
		MediaType string   `json:"mediaType"`
		Size      int64    `json:"size"`
		Digest    string   `json:"digest"`
		URLs      []string `json:"urls,omitempty"`
	}
	dockerManifest := struct {
		SchemaVersion int          `json:"schemaVersion"`
		MediaType     string       `json:"mediaType"`
		Config        descriptor   `json:"config"`
		Layers        []descriptor `json:"layers"`
	}{
		SchemaVersion: 2,
		MediaType:     string(types.DockerManifestSchema2),
		Config: descriptor{
			MediaType: string(types.DockerConfigJSON),
			Size:      manifest.Config.Size,
			Digest:    manifest.Config.Digest.String(),
		},
		Layers: []descriptor{{
			MediaType: string(types.DockerForeignLayer),
			Size:      manifest.Layers[0].Size,
			Digest:    manifest.Layers[0].Digest.String(),
			URLs:      []string{"https://example.com/foreign-layer-blob"},
		}},
	}
	data, err := json.Marshal(dockerManifest)
	if err != nil {
		t.Fatalf("marshal docker manifest: %v", err)
	}
	sum := sha256.Sum256(data)
	dockerDigest := "sha256:" + hex.EncodeToString(sum[:])
	if err := os.WriteFile(p.OCICacheBlob(hex.EncodeToString(sum[:])), data, 0644); err != nil {
		t.Fatalf("write docker manifest blob: %v", err)
	}

	cached, err := ImageFromCache(p, dockerDigest)
	if err != nil {
		t.Fatalf("ImageFromCache: %v", err)
	}

	gotManifest, err := cached.Manifest()
	if err != nil {
		t.Fatalf("Manifest: %v", err)
	}
	if len(gotManifest.Layers) != 1 || len(gotManifest.Layers[0].URLs) != 1 {
		t.Fatalf("foreign-layer urls not preserved: %+v", gotManifest.Layers)
	}
	if gotManifest.Layers[0].URLs[0] != "https://example.com/foreign-layer-blob" {
		t.Errorf("urls = %v, want preserved", gotManifest.Layers[0].URLs)
	}
	if gotManifest.Layers[0].MediaType != types.OCIRestrictedLayer {
		t.Errorf("foreign layer media type = %s, want %s", gotManifest.Layers[0].MediaType, types.OCIRestrictedLayer)
	}

	wantDigest, err := cached.Digest()
	if err != nil {
		t.Fatalf("Digest: %v", err)
	}
	raw, err := cached.RawManifest()
	if err != nil {
		t.Fatalf("RawManifest: %v", err)
	}
	wantSum := sha256.Sum256(raw)
	if wantDigest.Hex != hex.EncodeToString(wantSum[:]) {
		t.Errorf("digest %s does not match sha256 of converted manifest", wantDigest)
	}
}

func TestImageFromCacheDockerV2Conversion(t *testing.T) {
	p := tempPaths(t)
	img, err := random.Image(256, 1)
	if err != nil {
		t.Fatalf("random image: %v", err)
	}
	writeCacheImage(t, p, img)

	manifestData := dockerV2Manifest(t, img)
	sum := sha256.Sum256(manifestData)
	dockerDigest := "sha256:" + hex.EncodeToString(sum[:])
	if err := os.WriteFile(p.OCICacheBlob(hex.EncodeToString(sum[:])), manifestData, 0644); err != nil {
		t.Fatalf("write docker manifest blob: %v", err)
	}

	cached, err := ImageFromCache(p, dockerDigest)
	if err != nil {
		t.Fatalf("ImageFromCache: %v", err)
	}

	mediaType, err := cached.MediaType()
	if err != nil {
		t.Fatalf("media type: %v", err)
	}
	if mediaType != types.OCIManifestSchema1 {
		t.Errorf("media type = %s, want %s", mediaType, types.OCIManifestSchema1)
	}

	// The converted OCI manifest has a different digest than the Docker v2 input.
	gotDigest, err := cached.Digest()
	if err != nil {
		t.Fatalf("digest: %v", err)
	}
	if gotDigest.String() == dockerDigest {
		t.Error("converted digest should differ from the Docker v2 input digest")
	}

	manifest, err := cached.Manifest()
	if err != nil {
		t.Fatalf("manifest: %v", err)
	}
	if manifest.Config.MediaType != types.OCIConfigJSON {
		t.Errorf("config media type = %s, want %s", manifest.Config.MediaType, types.OCIConfigJSON)
	}
	for i, layer := range manifest.Layers {
		if layer.MediaType != types.OCILayer {
			t.Errorf("layer %d media type = %s, want %s", i, layer.MediaType, types.OCILayer)
		}
	}
}

// TestImageFromCacheBareHexDigest pins that a bare 64-hex digest (no
// "sha256:" prefix) resolves and yields a valid Digest(), not a parse error.
func TestImageFromCacheBareHexDigest(t *testing.T) {
	p := tempPaths(t)
	img, err := random.Image(256, 1)
	if err != nil {
		t.Fatalf("random image: %v", err)
	}
	img = mutate.MediaType(img, types.OCIManifestSchema1)
	digest := writeCacheImage(t, p, img)

	cached, err := ImageFromCache(p, strings.TrimPrefix(digest, "sha256:"))
	if err != nil {
		t.Fatalf("ImageFromCache(bare hex): %v", err)
	}
	got, err := cached.Digest()
	if err != nil {
		t.Fatalf("Digest: %v", err)
	}
	if got.String() != digest {
		t.Errorf("Digest = %s, want %s", got, digest)
	}
}
