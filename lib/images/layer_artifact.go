package images

import (
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/kernel/hypeman/lib/paths"
	"github.com/klauspost/compress/zstd"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/opencontainers/umoci/oci/layer"
	"golang.org/x/sys/unix"
)

const (
	layerRecordSchemaVersion = 3
	maxLayerUnpackedBytes    = 100 << 30

	// layerBuildTimeout bounds a shared layer build end to end. Builds are
	// detached from the initiating request's context, so this deadline is the
	// only thing that can abort a hung unpack and free the singleflight key.
	layerBuildTimeout = time.Hour
)

var errCorruptLayerRecord = errors.New("corrupt layer record")

// layerArtifact is the persisted record for one materialized layer artifact.
// The key is the compressed layer blob digest plus the artifact format. The
// encoding records the descriptor's decompression class so conflicting
// descriptors cannot overwrite one artifact path.
type layerArtifact struct {
	SchemaVersion  int       `json:"schema_version"`
	Digest         string    `json:"digest"` // compressed layer blob digest, sha256:...
	Encoding       string    `json:"encoding"`
	DiffID         string    `json:"diff_id,omitempty"`
	Format         string    `json:"format"`
	SizeBytes      int64     `json:"size_bytes"`      // artifact bytes on disk
	ArtifactDigest string    `json:"artifact_digest"` // sha256 of artifact bytes
	UnpackedBytes  int64     `json:"unpacked_bytes"`  // decompressed tar stream bytes
	CreatedAt      time.Time `json:"created_at"`
}

// validate checks a record read back from disk. The format fully determines
// the artifact options (erofs is always lz4-compressed, ext4 uncompressed),
// so only the format is stored.
func (a *layerArtifact) validate() error {
	if a.SchemaVersion != layerRecordSchemaVersion {
		return fmt.Errorf("unsupported schema version: %d", a.SchemaVersion)
	}
	if _, err := parseSHA256Digest(a.Digest); err != nil {
		return fmt.Errorf("invalid digest: %w", err)
	}
	if _, err := parseSHA256Digest(a.ArtifactDigest); err != nil {
		return fmt.Errorf("invalid artifact digest: %w", err)
	}
	if a.Format != string(FormatErofs) && a.Format != string(FormatExt4) {
		return fmt.Errorf("invalid format: %s", a.Format)
	}
	if a.Encoding != "raw" && a.Encoding != "gzip" && a.Encoding != "zstd" {
		return fmt.Errorf("invalid encoding: %s", a.Encoding)
	}
	if a.SizeBytes < 0 || a.UnpackedBytes < 0 {
		return fmt.Errorf("invalid size")
	}
	return nil
}

// matches decides whether the stored record satisfies a lookup. A descriptor
// without a DiffID (a manifest lookup that did not consult the image config)
// matches any record: the compressed digest content-addresses the blob, and
// unpackCachedLayer re-verifies the diff ID whenever one is supplied.
func (a *layerArtifact) matches(desc layerDescriptor) bool {
	if a.Digest != desc.Digest || a.Format != layerArtifactFormat() || a.Encoding != layerEncoding(desc.MediaType) {
		return false
	}
	return desc.DiffID == "" || a.DiffID == desc.DiffID
}

func layerEncoding(mediaType string) string {
	normalized := convertToOCIMediaType(mediaType)
	switch {
	case normalized == v1.MediaTypeImageLayerZstd, strings.HasSuffix(normalized, ".tar.zstd"):
		return "zstd"
	case normalized == v1.MediaTypeImageLayerGzip, strings.HasSuffix(normalized, ".tar.gzip"):
		return "gzip"
	default:
		return "raw"
	}
}

func layerArtifactFormat() string {
	switch DefaultImageFormat {
	case FormatErofs, FormatExt4:
		return string(DefaultImageFormat)
	default:
		return ""
	}
}

func layerArtifactPath(p *paths.Paths, layerHex string) string {
	return p.ImageLayerArtifactForFormat(layerHex, layerArtifactFormat())
}

func layerArtifactRecordPath(p *paths.Paths, layerHex string) string {
	return p.ImageLayerRecordForFormat(layerHex, layerArtifactFormat())
}

func layerDigestHex(value string) (string, error) {
	digest, err := parseSHA256Digest(value)
	if err != nil || digest.String() != value {
		return "", fmt.Errorf("invalid layer digest %q", value)
	}
	return digest.Encoded(), nil
}

// layerMapOptions preserves tar ownership when running as root. Otherwise
// umoci's rootless mode skips chown and stands in empty files for device nodes.
// Unlike unpackLayers in oci.go, which maps container root to the current
// user, this deliberately leaves ownership untouched as root: artifacts must
// keep the layer's on-disk ownership for later stacking.
func layerMapOptions() layer.MapOptions {
	return layer.MapOptions{Rootless: os.Geteuid() != 0}
}

// supportsLayerArtifacts reports whether the host can create the native
// overlayfs representation used by per-layer artifacts. Other platforms and
// rootless processes must compose the OCI layers into one rootfs instead.
func supportsLayerArtifacts() bool {
	return runtime.GOOS == "linux" && os.Geteuid() == 0
}

func probeLayerArtifactSupport(layersDir string) bool {
	if !supportsLayerArtifacts() || os.MkdirAll(layersDir, 0755) != nil {
		return false
	}
	probeDir, err := os.MkdirTemp(layersDir, ".probe-*")
	if err != nil {
		return false
	}
	defer os.RemoveAll(probeDir)

	whiteout := filepath.Join(probeDir, "whiteout")
	if err := unix.Mknod(whiteout, unix.S_IFCHR|0600, int(unix.Mkdev(0, 0))); err != nil {
		return false
	}
	if err := unix.Lsetxattr(probeDir, "trusted.overlay.opaque", []byte("y"), 0); err != nil {
		return false
	}
	value := make([]byte, 1)
	n, err := unix.Lgetxattr(probeDir, "trusted.overlay.opaque", value)
	return err == nil && n == 1 && value[0] == 'y'
}

func (m *manager) layerArtifactSupport() bool {
	return m.layers.artifactsSupported
}

func (m *manager) materializeLayerArtifact(ctx context.Context, desc layerDescriptor) (*layerArtifact, error) {
	return m.layers.materializeLayerArtifact(ctx, desc)
}

// layerArtifactOnDiskFormat is the extraction format for per-layer artifacts.
// Whiteouts become overlayfs whiteout inodes and opaque xattrs, which an
// overlayfs mount of stacked layers understands, so the artifact retains the
// layer's deletions without a private marker format. Callers must check
// supportsLayerArtifacts before materializing this format.
func layerArtifactOnDiskFormat() layer.OnDiskFormat {
	return layer.OverlayfsRootfs{MapOptions: layerMapOptions()}
}

// readLayerRecord loads the artifact record for a layer digest, if present.
// A missing record returns (nil, nil): the layer simply was never
// materialized.
func readLayerRecord(p *paths.Paths, layerHex string) (*layerArtifact, error) {
	recordPath := layerArtifactRecordPath(p, layerHex)
	info, err := os.Lstat(recordPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("stat layer record: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%w: layer record is not a regular file", errCorruptLayerRecord)
	}
	data, err := os.ReadFile(recordPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read layer record: %w", err)
	}
	var record layerArtifact
	if err := json.Unmarshal(data, &record); err != nil {
		return nil, fmt.Errorf("%w: unmarshal layer record: %v", errCorruptLayerRecord, err)
	}
	if err := record.validate(); err != nil {
		return nil, fmt.Errorf("%w: invalid layer record: %v", errCorruptLayerRecord, err)
	}
	return &record, nil
}

func discardLayerCache(p *paths.Paths, layerHex string) error {
	for _, path := range []string{
		layerArtifactRecordPath(p, layerHex),
		layerArtifactPath(p, layerHex),
	} {
		if err := removePath(path); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

func (s *layerStore) clearLayerCache(layerHex string) error {
	err := discardLayerCache(s.paths, layerHex)
	s.invalidateDiskUsageTotals()
	return err
}

// materializeLayerArtifact ensures a layer has a materialized artifact keyed
// by its blob digest, building it from the shared OCI cache blob when absent.
// The layer is unpacked into an isolated temp directory, converted to the
// default image format, and installed atomically. Normal failures remove the
// temp directory; a crash mid-build can leave a stale .unpack-* directory
// behind, which reconciliation landing with the pull integration is expected
// to sweep. No production caller yet: pull integration and
// composition land in later changes.
//
// Concurrent callers share one build. The build itself is detached from the
// initiating caller's cancellation so one cancelled pull cannot fail every
// other pull waiting on the same layer; each caller still returns as soon as
// its own context is done.
func (s *layerStore) materializeLayerArtifact(ctx context.Context, desc layerDescriptor) (*layerArtifact, error) {
	layerHex, err := layerDigestHex(desc.Digest)
	if err != nil {
		return nil, err
	}
	key := desc.Digest + "\x00" + layerArtifactFormat()
	result := s.flights.DoChan(key, func() (any, error) {
		// The build outlives the initiating request, so this deadline is its
		// only bound: without it a hung cache-blob read would wedge the
		// singleflight key, and every future caller for the layer, forever.
		buildCtx, cancel := context.WithTimeout(context.Background(), layerBuildTimeout)
		defer cancel()
		if s.builds != nil {
			select {
			case s.builds <- struct{}{}:
				defer func() { <-s.builds }()
			case <-buildCtx.Done():
				return nil, buildCtx.Err()
			}
		}
		return s.materializeLayerArtifactOnce(buildCtx, desc, layerHex)
	})
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case shared := <-result:
		if shared.Err != nil {
			return nil, shared.Err
		}
		artifact := shared.Val.(*layerArtifact)
		if !artifact.matches(desc) {
			return nil, fmt.Errorf("layer artifact metadata does not match descriptor %s", desc.Digest)
		}
		return artifact, nil
	}
}

func (s *layerStore) materializeLayerArtifactOnce(ctx context.Context, desc layerDescriptor, layerHex string) (*layerArtifact, error) {
	if layerArtifactFormat() == "" {
		return nil, fmt.Errorf("unsupported layer artifact format: %s", DefaultImageFormat)
	}
	if !s.artifactsSupported {
		return nil, fmt.Errorf("layer artifacts are unsupported on this host")
	}

	if record, err := readLayerRecord(s.paths, layerHex); err != nil {
		if !errors.Is(err, errCorruptLayerRecord) {
			return nil, err
		}
		if discardErr := s.clearLayerCache(layerHex); discardErr != nil {
			return nil, fmt.Errorf("discard corrupt layer cache: %w", discardErr)
		}
	} else if record != nil && record.Digest == desc.Digest && record.Format == layerArtifactFormat() && record.Encoding != layerEncoding(desc.MediaType) {
		return nil, fmt.Errorf("layer artifact encoding does not match descriptor %s", desc.Digest)
	} else if record != nil && record.matches(desc) {
		artifactInfo, statErr := os.Lstat(layerArtifactPath(s.paths, layerHex))
		if statErr == nil && artifactInfo.Mode().IsRegular() && record.SizeBytes > 0 && artifactInfo.Size() == record.SizeBytes {
			artifactDigest, digestErr := fileSHA256(ctx, layerArtifactPath(s.paths, layerHex))
			if digestErr == nil && artifactDigest == record.ArtifactDigest {
				return record, nil
			}
		}
		if discardErr := s.clearLayerCache(layerHex); discardErr != nil {
			return nil, fmt.Errorf("discard invalid layer cache: %w", discardErr)
		}
	}

	artifactPath := layerArtifactPath(s.paths, layerHex)
	if artifactInfo, statErr := os.Lstat(artifactPath); statErr == nil && !artifactInfo.Mode().IsRegular() {
		if discardErr := s.clearLayerCache(layerHex); discardErr != nil {
			return nil, fmt.Errorf("discard invalid layer artifact: %w", discardErr)
		}
	}

	layerDir := s.paths.ImageLayerDir(layerHex)
	if err := os.MkdirAll(layerDir, 0755); err != nil {
		return nil, fmt.Errorf("create layer directory: %w", err)
	}
	unpackDir, err := os.MkdirTemp(layerDir, ".unpack-*")
	if err != nil {
		return nil, fmt.Errorf("create unpack directory: %w", err)
	}
	s.beginLayerBuild()
	defer func() {
		if err := removePath(unpackDir); err != nil {
			slog.Warn("failed to remove layer unpack directory", "dir", unpackDir, "error", err)
		}
		s.endLayerBuild()
	}()

	stats, err := unpackCachedLayer(ctx, s.paths, desc, unpackDir, layerArtifactOnDiskFormat())
	if err != nil {
		return nil, err
	}
	record, err := s.installLayerArtifact(ctx, desc, layerHex, unpackDir, stats)
	if err != nil {
		s.invalidateDiskUsageTotals()
		return nil, err
	}
	s.invalidateDiskUsageTotals()
	return record, nil
}

func (s *layerStore) installLayerArtifact(ctx context.Context, desc layerDescriptor, layerHex, unpackDir string, stats *unpackStats) (*layerArtifact, error) {
	record := &layerArtifact{
		SchemaVersion: layerRecordSchemaVersion,
		Digest:        desc.Digest,
		Encoding:      layerEncoding(desc.MediaType),
		DiffID:        stats.diffID,
		Format:        layerArtifactFormat(),
		UnpackedBytes: stats.unpackedBytes,
		CreatedAt:     time.Now(),
	}

	if err := installAtomically(layerArtifactPath(s.paths, layerHex), func(path string) error {
		size, err := ExportRootfsWithContext(ctx, unpackDir, path, DefaultImageFormat)
		if err != nil {
			return err
		}
		record.SizeBytes = size
		record.ArtifactDigest, err = fileSHA256(ctx, path)
		return err
	}); err != nil {
		return nil, fmt.Errorf("install layer artifact %s: %w", desc.Digest, err)
	}

	data, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal layer record: %w", err)
	}
	if err := writeJSONAtomic(layerArtifactRecordPath(s.paths, layerHex), data); err != nil {
		_ = os.Remove(layerArtifactPath(s.paths, layerHex))
		return nil, fmt.Errorf("write layer record: %w", err)
	}
	return record, nil
}

type unpackStats struct {
	unpackedBytes int64
	diffID        string
	blobDigest    string // sha256 of the compressed bytes as read
}

func fileSHA256(ctx context.Context, path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()

	hash := sha256.New()
	if _, err := io.Copy(hash, contextReader{ctx: ctx, reader: file}); err != nil {
		return "", err
	}
	return fmt.Sprintf("sha256:%x", hash.Sum(nil)), nil
}

// contextReader fails reads once ctx is done so a cancelled caller stops a
// long extraction instead of running it to completion.
type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}

// unpackCachedLayer locates desc's blob in the shared OCI cache, unpacks it
// into dest, and verifies both the blob digest and the diff ID when the
// descriptor carries one. The caller must have validated desc.Digest.
func unpackCachedLayer(ctx context.Context, p *paths.Paths, desc layerDescriptor, dest string, onDisk layer.OnDiskFormat) (*unpackStats, error) {
	blobPath := p.OCICacheBlob(strings.TrimPrefix(desc.Digest, "sha256:"))
	if _, err := os.Stat(blobPath); err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("layer blob missing from oci cache: %s", desc.Digest)
		}
		return nil, fmt.Errorf("stat layer blob: %w", err)
	}
	stats, err := unpackLayerBlob(ctx, blobPath, desc.MediaType, dest, onDisk)
	if err != nil {
		return nil, fmt.Errorf("unpack layer %s: %w", desc.Digest, err)
	}
	if desc.Digest != "" && stats.blobDigest != desc.Digest {
		return nil, fmt.Errorf("layer blob digest mismatch: got %s, want %s", stats.blobDigest, desc.Digest)
	}
	if desc.DiffID != "" && stats.diffID != desc.DiffID {
		return nil, fmt.Errorf("layer %s diff id mismatch: got %s, want %s", desc.Digest, stats.diffID, desc.DiffID)
	}
	return stats, nil
}

// unpackLayerBlob extracts one compressed layer blob into dest with umoci,
// which confines every entry to dest and interprets whiteouts per onDisk.
// The compressed stream is hashed to verify the blob digest, the decompressed
// stream for the diff ID, and capped in size.
func unpackLayerBlob(ctx context.Context, blobPath, mediaType, dest string, onDisk layer.OnDiskFormat) (*unpackStats, error) {
	if err := os.MkdirAll(dest, 0755); err != nil {
		return nil, fmt.Errorf("create extraction root: %w", err)
	}
	blob, err := os.Open(blobPath)
	if err != nil {
		return nil, fmt.Errorf("open blob: %w", err)
	}
	defer blob.Close()

	blobHash := sha256.New()
	compressed := io.TeeReader(contextReader{ctx: ctx, reader: blob}, blobHash)
	reader, closer, err := decompressLayer(compressed, mediaType)
	if err != nil {
		return nil, err
	}
	defer closer.Close()

	hash := sha256.New()
	limited := &io.LimitedReader{R: contextReader{ctx: ctx, reader: reader}, N: maxLayerUnpackedBytes + 1}
	hashed := io.TeeReader(limited, hash)
	if err := layer.UnpackLayer(dest, hashed, &layer.UnpackOptions{OnDiskFormat: onDisk}); err != nil {
		return nil, err
	}
	if _, err := io.Copy(io.Discard, hashed); err != nil {
		return nil, fmt.Errorf("drain layer: %w", err)
	}
	if _, err := io.Copy(io.Discard, compressed); err != nil {
		return nil, fmt.Errorf("drain blob: %w", err)
	}
	if limited.N == 0 {
		return nil, fmt.Errorf("layer exceeds maximum unpacked size of %d bytes", maxLayerUnpackedBytes)
	}
	return &unpackStats{
		unpackedBytes: maxLayerUnpackedBytes + 1 - limited.N,
		diffID:        fmt.Sprintf("sha256:%x", hash.Sum(nil)),
		blobDigest:    fmt.Sprintf("sha256:%x", blobHash.Sum(nil)),
	}, nil
}

// decompressLayer wraps the blob in the reader for its layer media type. Both
// OCI-style suffixes (+gzip, +zstd) and docker-style media types (tar.gzip,
// tar.zstd) are matched so neither encoding falls through to the raw path.
func decompressLayer(r io.Reader, mediaType string) (io.Reader, io.Closer, error) {
	switch layerEncoding(mediaType) {
	case "zstd":
		decoder, err := zstd.NewReader(r)
		if err != nil {
			return nil, nil, fmt.Errorf("zstd reader: %w", err)
		}
		decodeCloser := decoder.IOReadCloser()
		return decodeCloser, decodeCloser, nil
	case "gzip":
		gz, err := gzip.NewReader(r)
		if err != nil {
			return nil, nil, fmt.Errorf("gzip reader: %w", err)
		}
		return gz, gz, nil
	default:
		return r, io.NopCloser(r), nil
	}
}

// removePath removes a tree that may contain read-only directories restored
// from layer metadata.
func removePath(path string) error {
	if err := os.RemoveAll(path); err == nil || os.IsNotExist(err) {
		return nil
	}
	if err := makeTreeWritable(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := os.RemoveAll(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func makeTreeWritable(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil
	}
	if err := os.Chmod(path, info.Mode().Perm()|0700); err != nil {
		return err
	}
	return filepath.WalkDir(path, func(p string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() && entry.Type()&os.ModeSymlink == 0 {
			info, err := entry.Info()
			if err != nil {
				return err
			}
			return os.Chmod(p, info.Mode().Perm()|0700)
		}
		return nil
	})
}
