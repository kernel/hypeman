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
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/kernel/hypeman/lib/paths"
	"github.com/klauspost/compress/zstd"
	"github.com/opencontainers/umoci/oci/layer"
)

// whiteoutPrefix marks OCI whiteout entries (".wh.<name>" and ".wh..wh..opq").
// umoci interprets them during extraction: DirRootfs applies them against the
// tree being composed, OverlayfsRootfs converts them to overlayfs whiteout
// inodes and opaque xattrs so per-layer artifacts can later be stacked.
const whiteoutPrefix = ".wh."

const (
	layerRecordSchemaVersion = 1
	maxLayerUnpackedBytes    = 100 << 30
)

var errCorruptLayerRecord = errors.New("corrupt layer record")

// layerArtifact is the persisted record for one materialized layer artifact.
// The key is the compressed layer blob digest plus the artifact format, so the
// same layer can coexist in several materializations.
type layerArtifact struct {
	SchemaVersion int       `json:"schema_version"`
	Digest        string    `json:"digest"` // compressed layer blob digest, sha256:...
	DiffID        string    `json:"diff_id,omitempty"`
	Format        string    `json:"format"`
	SizeBytes     int64     `json:"size_bytes"`     // artifact bytes on disk
	UnpackedBytes int64     `json:"unpacked_bytes"` // decompressed tar stream bytes
	CreatedAt     time.Time `json:"created_at"`
}

// validate checks a record read back from disk. The format fully determines
// the artifact options (erofs is always lz4-compressed, ext4 uncompressed),
// so only the format is stored.
func (a *layerArtifact) validate() error {
	if a.SchemaVersion != layerRecordSchemaVersion {
		return fmt.Errorf("unsupported schema version: %d", a.SchemaVersion)
	}
	if a.Digest == "" {
		return fmt.Errorf("missing digest")
	}
	if a.Format != string(FormatErofs) && a.Format != string(FormatExt4) {
		return fmt.Errorf("invalid format: %s", a.Format)
	}
	if a.SizeBytes < 0 || a.UnpackedBytes < 0 {
		return fmt.Errorf("invalid size")
	}
	return nil
}

func (a *layerArtifact) matches(desc layerDescriptor) bool {
	if a.Digest != desc.Digest || a.Format != layerArtifactFormat() {
		return false
	}
	return desc.DiffID == "" || a.DiffID == desc.DiffID
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

// layerMapOptions preserves tar ownership when running as root. Otherwise
// umoci's rootless mode skips chown and stands in empty files for device nodes.
func layerMapOptions() layer.MapOptions {
	return layer.MapOptions{Rootless: os.Geteuid() != 0}
}

// layerArtifactOnDiskFormat is the extraction format for per-layer artifacts.
// Whiteouts become overlayfs whiteout inodes and opaque xattrs, the form an
// overlayfs mount of stacked layers understands, so the artifact retains the
// layer's deletions without a private marker format.
func layerArtifactOnDiskFormat() layer.OnDiskFormat {
	return layer.OverlayfsRootfs{MapOptions: layerMapOptions()}
}

// composeOnDiskFormat applies whiteouts against the tree being composed.
func composeOnDiskFormat() layer.OnDiskFormat {
	return layer.DirRootfs{MapOptions: layerMapOptions()}
}

// readLayerRecord loads the artifact record for a layer digest, if present.
// A missing record returns (nil, nil): the layer simply was never
// materialized.
func readLayerRecord(p *paths.Paths, layerHex string) (*layerArtifact, error) {
	data, err := os.ReadFile(layerArtifactRecordPath(p, layerHex))
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
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

// materializeLayerArtifact ensures a layer has a materialized artifact keyed
// by its blob digest, building it from the shared OCI cache blob when absent.
// The layer is unpacked into an isolated temp directory, converted to the
// default image format, and installed atomically. Normal failures remove the
// temp directory; lifecycle reconciliation removes stale temp directories after
// an interrupted build. No production caller yet: pull integration and
// composition land in later changes.
func (m *manager) materializeLayerArtifact(ctx context.Context, desc layerDescriptor) (*layerArtifact, error) {
	key := desc.Digest + "\x00" + layerArtifactFormat()
	result := m.layerFlights.DoChan(key, func() (any, error) {
		return m.materializeLayerArtifactOnce(ctx, desc)
	})
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case shared := <-result:
		if shared.Err != nil {
			return nil, shared.Err
		}
		return shared.Val.(*layerArtifact), nil
	}
}

func (m *manager) materializeLayerArtifactOnce(ctx context.Context, desc layerDescriptor) (*layerArtifact, error) {
	if layerArtifactFormat() == "" {
		return nil, fmt.Errorf("unsupported layer artifact format: %s", DefaultImageFormat)
	}
	layerHex := strings.TrimPrefix(desc.Digest, "sha256:")
	if err := paths.ValidatePathComponent(layerHex); err != nil {
		return nil, fmt.Errorf("invalid layer digest %s: %w", desc.Digest, err)
	}

	if record, err := readLayerRecord(m.paths, layerHex); err != nil {
		if !errors.Is(err, errCorruptLayerRecord) {
			return nil, err
		}
		if discardErr := discardLayerCache(m.paths, layerHex); discardErr != nil {
			return nil, fmt.Errorf("discard corrupt layer cache: %w", discardErr)
		}
	} else if record != nil && record.matches(desc) {
		if _, statErr := os.Stat(layerArtifactPath(m.paths, layerHex)); statErr == nil {
			return record, nil
		}
		// Record without artifact: rebuild below.
	}

	layerDir := m.paths.ImageLayerDir(layerHex)
	if err := os.MkdirAll(layerDir, 0755); err != nil {
		return nil, fmt.Errorf("create layer directory: %w", err)
	}
	unpackDir, err := os.MkdirTemp(layerDir, ".unpack-*")
	if err != nil {
		return nil, fmt.Errorf("create unpack directory: %w", err)
	}
	defer removePath(unpackDir)

	stats, err := unpackCachedLayer(ctx, m.paths.SystemOCICache(), desc, unpackDir, layerArtifactOnDiskFormat())
	if err != nil {
		return nil, err
	}
	return m.installLayerArtifact(ctx, desc, layerHex, unpackDir, stats)
}

func (m *manager) installLayerArtifact(ctx context.Context, desc layerDescriptor, layerHex, unpackDir string, stats *unpackStats) (*layerArtifact, error) {
	record := &layerArtifact{
		SchemaVersion: layerRecordSchemaVersion,
		Digest:        desc.Digest,
		DiffID:        stats.diffID,
		Format:        layerArtifactFormat(),
		UnpackedBytes: stats.unpackedBytes,
		CreatedAt:     time.Now(),
	}

	if err := installAtomically(layerArtifactPath(m.paths, layerHex), func(path string) error {
		var size int64
		var convErr error
		if DefaultImageFormat == FormatErofs {
			size, convErr = convertToErofsContext(ctx, unpackDir, path)
		} else {
			size, convErr = ExportRootfs(unpackDir, path, DefaultImageFormat)
		}
		if convErr != nil {
			return convErr
		}
		record.SizeBytes = size
		return nil
	}); err != nil {
		return nil, fmt.Errorf("install layer artifact %s: %w", desc.Digest, err)
	}

	data, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal layer record: %w", err)
	}
	if err := writeJSONAtomic(layerArtifactRecordPath(m.paths, layerHex), data); err != nil {
		_ = os.Remove(layerArtifactPath(m.paths, layerHex))
		return nil, fmt.Errorf("write layer record: %w", err)
	}
	return record, nil
}

type unpackStats struct {
	unpackedBytes int64
	diffID        string
}

// unpackCachedLayer locates desc's blob in the shared OCI cache, unpacks it
// into dest, and verifies the diff ID when the descriptor carries one.
func unpackCachedLayer(ctx context.Context, cacheDir string, desc layerDescriptor, dest string, onDisk layer.OnDiskFormat) (*unpackStats, error) {
	layerHex := strings.TrimPrefix(desc.Digest, "sha256:")
	if err := paths.ValidatePathComponent(layerHex); err != nil {
		return nil, fmt.Errorf("invalid layer digest: %s", desc.Digest)
	}
	blobPath := filepath.Join(cacheDir, "blobs", "sha256", layerHex)
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
	if desc.DiffID != "" && stats.diffID != desc.DiffID {
		return nil, fmt.Errorf("layer %s diff id mismatch: got %s, want %s", desc.Digest, stats.diffID, desc.DiffID)
	}
	return stats, nil
}

// unpackLayerBlob extracts one compressed layer blob into dest with umoci,
// which confines every entry to dest and interprets whiteouts per onDisk.
// The decompressed stream is hashed for the diff ID and capped in size.
func unpackLayerBlob(ctx context.Context, blobPath, mediaType, dest string, onDisk layer.OnDiskFormat) (*unpackStats, error) {
	if err := os.MkdirAll(dest, 0755); err != nil {
		return nil, fmt.Errorf("create extraction root: %w", err)
	}
	blob, err := os.Open(blobPath)
	if err != nil {
		return nil, fmt.Errorf("open blob: %w", err)
	}
	defer blob.Close()

	reader, closer, err := decompressLayer(blob, mediaType)
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
	if limited.N == 0 {
		return nil, fmt.Errorf("layer exceeds maximum unpacked size of %d bytes", maxLayerUnpackedBytes)
	}
	return &unpackStats{
		unpackedBytes: maxLayerUnpackedBytes + 1 - limited.N,
		diffID:        fmt.Sprintf("sha256:%x", hash.Sum(nil)),
	}, nil
}

// decompressLayer wraps the blob in the reader for its layer media type. Both
// OCI-style suffixes (+gzip, +zstd) and docker-style media types (tar.gzip,
// tar.zstd) are matched so neither encoding falls through to the raw path.
func decompressLayer(blob *os.File, mediaType string) (io.Reader, io.Closer, error) {
	switch {
	case strings.HasSuffix(mediaType, "+zstd"), strings.Contains(mediaType, "tar.zstd"):
		decoder, err := zstd.NewReader(blob)
		if err != nil {
			return nil, nil, fmt.Errorf("zstd reader: %w", err)
		}
		return decoder, multiCloser{decoder.IOReadCloser(), blob}, nil
	case strings.HasSuffix(mediaType, "+gzip"), strings.Contains(mediaType, "tar.gzip"):
		gz, err := gzip.NewReader(blob)
		if err != nil {
			return nil, nil, fmt.Errorf("gzip reader: %w", err)
		}
		return gz, multiCloser{gz, blob}, nil
	default:
		return blob, blob, nil
	}
}

type multiCloser []io.Closer

func (c multiCloser) Close() error {
	var firstErr error
	for _, closer := range c {
		if err := closer.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// removePath removes a tree that may contain read-only directories restored
// from layer metadata.
func removePath(path string) error {
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
