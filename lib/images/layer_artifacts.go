// Per-layer artifact export.
//
// ExportLayerArtifacts reads an image's OCI layers from the shared OCI cache
// and converts each supported layer into a content-addressed erofs artifact
// under images/layers/<diff-id>/. Artifacts are keyed by the layer's diff ID
// (the sha256 of its uncompressed tar, from the image config's rootfs.diff_ids),
// which is the canonical identity of a layer's content: the same layer shared
// by any number of images converts once.
//
// Scope: this is the reusable exporter only. It does not change the flattened
// image build path (buildImage still produces one rootfs per image digest),
// does not record anything in image metadata, and does not compose artifacts
// at runtime. Known limitations that a future composition step must resolve
// are documented on ExportLayerArtifacts.
package images

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"runtime"
	"strings"
	"time"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/types"
	"github.com/kernel/hypeman/lib/ocicache"
	"github.com/kernel/hypeman/lib/paths"
	"github.com/opencontainers/umoci/oci/layer"
)

// ErrLayerArtifactsUnsupported is returned when this host cannot produce
// erofs layer artifacts (non-Linux host or mkfs.erofs not installed).
// Callers may treat it as a soft skip.
var ErrLayerArtifactsUnsupported = errors.New("layer artifact export unsupported")

// LayerArtifact describes one converted layer.
type LayerArtifact struct {
	Index        int    // position of the layer in the image manifest
	LayerDigest  string // sha256:... of the compressed layer blob
	DiffID       string // sha256:... of the uncompressed layer content (artifact key)
	ArtifactPath string
	SizeBytes    int64
	Reused       bool // artifact was already present from a previous export
}

// SkippedLayer describes a layer that was not converted, with the reason.
type SkippedLayer struct {
	Index       int
	LayerDigest string
	Reason      string
}

// LayerExportReport is the result of exporting one image's layers.
type LayerExportReport struct {
	ImageDigest string
	Artifacts   []LayerArtifact
	Skipped     []SkippedLayer
}

// layerArtifactMetadata is persisted beside each artifact and is the
// contract for reading it back: the source blob identity plus the filesystem
// format and the options it was built with, so a consumer knows how to mount
// or interpret the artifact without re-deriving anything.
type layerArtifactMetadata struct {
	LayerDigest string       `json:"layer_digest"`
	DiffID      string       `json:"diff_id"`
	Format      ExportFormat `json:"format"`
	Compression string       `json:"compression"` // erofs -z algorithm
	SectorSize  int64        `json:"sector_size"` // artifact padded to this alignment
	SizeBytes   int64        `json:"size_bytes"`
	CreatedAt   time.Time    `json:"created_at"`
}

// ExportLayerArtifacts converts the layers of the cached image identified by
// imageDigest into content-addressed erofs artifacts.
//
// Behavior:
//   - Layers are identified by diff ID; the uncompressed stream is hashed
//     during unpack and must match the config's rootfs.diff_ids entry, the
//     same integrity check umoci applies during a full unpack.
//   - Layers with an unsupported media type (anything ocicache cannot
//     decompress, e.g. zstd) are reported in Skipped, not errors.
//   - Layers carrying OCI deletion semantics are reported in Skipped, not
//     converted: whiteout entries (.wh.<name>) and opaque-directory markers
//     (.wh..wh..opq) delete content that lives in earlier layers, and a
//     standalone read-only artifact cannot express that. A raw tar-to-erofs
//     conversion of such a layer would silently drop the deletions, so the
//     exporter refuses it explicitly instead.
//   - Layers that cannot be unpacked standalone are also reported in
//     Skipped. This happens when a layer's tar references state from earlier
//     layers, most commonly hardlinks to files introduced by a lower layer.
//     Their unpacked content is removed; no partial artifact is installed.
//   - All other failures (missing blobs, diff ID mismatch, mkfs.erofs errors)
//     abort the export. Artifacts installed before the failure are valid and
//     reusable because they are content-addressed.
//
// Known limitations, deferred until layer composition is designed:
//   - Artifacts hold only what a layer adds; layers that delete are skipped
//     (above). Composing images from per-layer artifacts needs a format that
//     can carry deletions (overlayfs-style or custom), which has not been
//     chosen yet.
//   - The mapping from an image to its ordered artifact list is returned to
//     the caller but not persisted; imageMetadata gains layer fields once a
//     composition consumer exists.
//   - Artifacts are not reference-counted against the OCI cache GC. This is
//     safe today because an artifact is self-contained once written, but a
//     GC policy for images/layers is future work.
func ExportLayerArtifacts(ctx context.Context, p *paths.Paths, imageDigest string) (*LayerExportReport, error) {
	if runtime.GOOS != "linux" {
		return nil, fmt.Errorf("%w: erofs artifacts require a Linux guest kernel", ErrLayerArtifactsUnsupported)
	}
	if _, err := exec.LookPath("mkfs.erofs"); err != nil {
		return nil, fmt.Errorf("%w: mkfs.erofs not installed: %s", ErrLayerArtifactsUnsupported, err)
	}

	img, err := ocicache.ImageFromCache(p, imageDigest)
	if err != nil {
		return nil, err
	}
	manifest, err := img.Manifest()
	if err != nil {
		return nil, fmt.Errorf("read manifest: %w", err)
	}
	config, err := img.ConfigFile()
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	if config.RootFS.Type != "layers" {
		return nil, fmt.Errorf("unsupported rootfs.type: %s", config.RootFS.Type)
	}
	if len(config.RootFS.DiffIDs) != len(manifest.Layers) {
		return nil, fmt.Errorf(
			"config rootfs.diff_ids has %d entries but manifest has %d layers",
			len(config.RootFS.DiffIDs),
			len(manifest.Layers),
		)
	}

	digestHex := normalizeDigestHex(imageDigest)
	report := &LayerExportReport{
		ImageDigest: "sha256:" + digestHex,
		Artifacts:   make([]LayerArtifact, 0, len(manifest.Layers)),
		Skipped:     make([]SkippedLayer, 0),
	}

	for i, desc := range manifest.Layers {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		diffID := config.RootFS.DiffIDs[i]

		if reason, ok := supportedLayerMediaType(desc.MediaType); !ok {
			report.Skipped = append(report.Skipped, SkippedLayer{
				Index:       i,
				LayerDigest: desc.Digest.String(),
				Reason:      reason,
			})
			continue
		}

		artifact, reused, err := exportLayerArtifact(p, img, i, desc, diffID)
		if err != nil {
			var skipErr *layerSkipError
			if errors.As(err, &skipErr) {
				report.Skipped = append(report.Skipped, SkippedLayer{
					Index:       i,
					LayerDigest: desc.Digest.String(),
					Reason:      skipErr.Error(),
				})
				continue
			}
			return nil, fmt.Errorf("export layer %d (%s): %w", i, desc.Digest, err)
		}
		artifact.Reused = reused
		report.Artifacts = append(report.Artifacts, artifact)
	}

	return report, nil
}

// layerSkipError marks a layer that cannot be represented as a standalone
// artifact (deletion semantics, cross-layer references, unsupported media).
// The exporter reports these in Skipped instead of aborting the whole image.
type layerSkipError struct {
	reason string
	cause  error
}

func (e *layerSkipError) Error() string {
	if e.cause != nil {
		return e.reason + ": " + e.cause.Error()
	}
	return e.reason
}

func (e *layerSkipError) Unwrap() error { return e.cause }

// whiteoutPrefix and opaqueWhiteout follow the OCI layer conventions:
// ".wh.<name>" deletes <name> from lower layers and ".wh..wh..opq" marks its
// containing directory opaque (all lower-layer children deleted).
const (
	whiteoutPrefix = ".wh."
	opaqueWhiteout = ".wh..wh..opq"
)

// firstWhiteoutMarker scans the uncompressed layer tar and returns the name
// of the first OCI whiteout or opaque-directory entry, or "" when the layer
// carries no deletion semantics. The blob is local; the unpack reads it again
// afterwards. The scan short-circuits on the first marker.
func firstWhiteoutMarker(l v1.Layer) (string, error) {
	rc, err := l.Uncompressed()
	if err != nil {
		return "", err
	}
	defer rc.Close()
	tr := tar.NewReader(rc)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return "", nil
		}
		if err != nil {
			return "", fmt.Errorf("read layer tar: %w", err)
		}
		if base := path.Base(hdr.Name); strings.HasPrefix(base, whiteoutPrefix) {
			return hdr.Name, nil
		}
	}
}

// supportedLayerMediaType reports whether the OCI cache can serve a layer
// media type uncompressed; when not, it returns the skip reason. Docker v2
// types are already converted to OCI by ocicache, but both families are
// accepted here.
func supportedLayerMediaType(mediaType types.MediaType) (string, bool) {
	switch mediaType {
	case types.OCILayer, types.OCIUncompressedLayer, types.OCIRestrictedLayer,
		types.DockerLayer, types.DockerUncompressedLayer:
		return "", true
	default:
		return fmt.Sprintf("unsupported layer media type %s", mediaType), false
	}
}

func normalizeDigestHex(digest string) string {
	return strings.TrimPrefix(digest, "sha256:")
}

// exportLayerArtifact converts one layer, returning the installed artifact.
// The bool result reports whether an existing artifact was reused.
func exportLayerArtifact(p *paths.Paths, img v1.Image, index int, desc v1.Descriptor, diffID v1.Hash) (LayerArtifact, bool, error) {
	artifactPath := p.LayerArtifactPath(diffID.Hex)
	if info, err := os.Stat(artifactPath); err == nil {
		// Content-addressed and installed atomically: presence means complete.
		// Heal metadata lost to a crash between the artifact and metadata
		// installs, but keep an existing file so CreatedAt stays truthful.
		metadataPath := p.LayerArtifactMetadata(diffID.Hex)
		if _, err := os.Stat(metadataPath); err != nil {
			if err := writeLayerArtifactMetadata(p, desc, diffID, info.Size()); err != nil {
				return LayerArtifact{}, false, err
			}
		}
		return LayerArtifact{
			Index:        index,
			LayerDigest:  desc.Digest.String(),
			DiffID:       diffID.String(),
			ArtifactPath: artifactPath,
			SizeBytes:    info.Size(),
		}, true, nil
	}

	layerReader, err := img.LayerByDigest(desc.Digest)
	if err != nil {
		return LayerArtifact{}, false, fmt.Errorf("find layer in cache: %w", err)
	}

	// Refuse layers with OCI deletion semantics up front: unpacking them
	// standalone would apply their whiteouts against an empty root and drop
	// the deletions silently.
	marker, err := firstWhiteoutMarker(layerReader)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return LayerArtifact{}, false, fmt.Errorf("layer blob missing from OCI cache: %w", err)
		}
		return LayerArtifact{}, false, fmt.Errorf("scan layer for whiteouts: %w", err)
	}
	if marker != "" {
		return LayerArtifact{}, false, &layerSkipError{
			reason: fmt.Sprintf(
				"layer contains OCI whiteout or opaque-directory marker %q: deletions of lower-layer content cannot be represented in a standalone artifact",
				marker,
			),
		}
	}

	uncompressed, err := layerReader.Uncompressed()
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return LayerArtifact{}, false, fmt.Errorf("layer blob missing from OCI cache: %w", err)
		}
		return LayerArtifact{}, false, fmt.Errorf("open layer: %w", err)
	}

	if err := os.MkdirAll(p.LayerArtifactsDir(), 0755); err != nil {
		uncompressed.Close()
		return LayerArtifact{}, false, fmt.Errorf("create layer artifacts dir: %w", err)
	}
	unpackDir, err := os.MkdirTemp(p.LayerArtifactsDir(), ".unpack-*")
	if err != nil {
		uncompressed.Close()
		return LayerArtifact{}, false, fmt.Errorf("create unpack dir: %w", err)
	}
	defer os.RemoveAll(unpackDir)

	// Hash the uncompressed stream as it unpacks so the result can be
	// verified against the diff ID declared in the image config.
	hasher := sha256.New()
	if err := layer.UnpackLayer(unpackDir, io.TeeReader(uncompressed, hasher), rootlessUnpackOptions()); err != nil {
		uncompressed.Close()
		return LayerArtifact{}, false, &layerSkipError{reason: "cannot unpack layer standalone", cause: err}
	}
	if err := uncompressed.Close(); err != nil {
		return LayerArtifact{}, false, fmt.Errorf("read layer: %w", err)
	}

	if got := hex.EncodeToString(hasher.Sum(nil)); got != diffID.Hex {
		return LayerArtifact{}, false, fmt.Errorf("diff ID mismatch: unpacked sha256:%s, config declares %s", got, diffID.String())
	}

	var sizeBytes int64
	if err := installAtomically(artifactPath, func(tempPath string) error {
		var err error
		sizeBytes, err = ExportRootfs(unpackDir, tempPath, FormatErofs)
		return err
	}); err != nil {
		return LayerArtifact{}, false, fmt.Errorf("convert to erofs: %w", err)
	}

	if err := writeLayerArtifactMetadata(p, desc, diffID, sizeBytes); err != nil {
		return LayerArtifact{}, false, fmt.Errorf("install artifact metadata: %w", err)
	}

	return LayerArtifact{
		Index:        index,
		LayerDigest:  desc.Digest.String(),
		DiffID:       diffID.String(),
		ArtifactPath: artifactPath,
		SizeBytes:    sizeBytes,
	}, false, nil
}

// writeLayerArtifactMetadata installs metadata.json beside the artifact it
// describes. It serves both the fresh-install and the reuse-heal paths.
func writeLayerArtifactMetadata(p *paths.Paths, desc v1.Descriptor, diffID v1.Hash, sizeBytes int64) error {
	meta := layerArtifactMetadata{
		LayerDigest: desc.Digest.String(),
		DiffID:      diffID.String(),
		Format:      FormatErofs,
		Compression: ErofsCompression,
		SectorSize:  sectorSize,
		SizeBytes:   sizeBytes,
		CreatedAt:   time.Now(),
	}
	return installAtomically(p.LayerArtifactMetadata(diffID.Hex), func(tempPath string) error {
		data, err := json.MarshalIndent(&meta, "", "  ")
		if err != nil {
			return fmt.Errorf("marshal artifact metadata: %w", err)
		}
		return os.WriteFile(tempPath, data, 0644)
	})
}
