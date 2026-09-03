package images

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/kernel/hypeman/lib/paths"
	"github.com/klauspost/compress/zstd"
	"golang.org/x/sys/unix"
)

// OCI whiteout marker files. A ".wh.<name>" entry in directory D removes
// "<name>" from D as inherited from lower layers; a ".wh..wh..opq" entry marks
// its directory opaque, hiding everything below it from lower layers. These
// are tar-level conventions: they do not compose on overlayfs by themselves,
// so composition must interpret them explicitly (see applyLayerTree).
const (
	whiteoutPrefix = ".wh."
	opaqueWhiteout = ".wh..wh..opq"
)

const (
	layerRecordSchemaVersion = 1
	maxLayerEntries          = 1_000_000
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
	SizeBytes     int64     `json:"size_bytes"` // artifact bytes on disk
	UnpackedBytes int64     `json:"unpacked_bytes"`
	Entries       int       `json:"entries"`
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
	if a.SizeBytes < 0 || a.UnpackedBytes < 0 || a.Entries < 0 {
		return fmt.Errorf("invalid size or entry counts")
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
func (m *manager) materializeLayerArtifact(desc layerDescriptor) (*layerArtifact, error) {
	return m.materializeLayerArtifactContext(context.Background(), desc)
}

func (m *manager) materializeLayerArtifactContext(ctx context.Context, desc layerDescriptor) (*layerArtifact, error) {
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

	blobPath := m.paths.OCICacheBlob(layerHex)
	if _, err := os.Stat(blobPath); err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("layer blob missing from oci cache: %s", desc.Digest)
		}
		return nil, fmt.Errorf("stat layer blob: %w", err)
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

	stats, err := unpackLayerBlobContext(ctx, blobPath, desc.MediaType, unpackDir)
	if err != nil {
		return nil, fmt.Errorf("unpack layer %s: %w", desc.Digest, err)
	}
	if desc.DiffID != "" && stats.diffID != desc.DiffID {
		return nil, fmt.Errorf("layer %s diff id mismatch: got %s, want %s", desc.Digest, stats.diffID, desc.DiffID)
	}

	return m.installLayerArtifactContext(ctx, desc, layerHex, unpackDir, stats)
}

func (m *manager) installLayerArtifact(desc layerDescriptor, layerHex, unpackDir string, stats *unpackStats) (*layerArtifact, error) {
	return m.installLayerArtifactContext(context.Background(), desc, layerHex, unpackDir, stats)
}

func (m *manager) installLayerArtifactContext(ctx context.Context, desc layerDescriptor, layerHex, unpackDir string, stats *unpackStats) (*layerArtifact, error) {
	record := &layerArtifact{
		SchemaVersion: layerRecordSchemaVersion,
		Digest:        desc.Digest,
		DiffID:        stats.diffID,
		Format:        layerArtifactFormat(),
		UnpackedBytes: stats.unpackedBytes,
		Entries:       stats.entries,
		CreatedAt:     time.Now(),
	}

	if err := installAtomically(layerArtifactPath(m.paths, layerHex), func(path string) error {
		// The artifact intentionally retains .wh. marker files so
		// composition can re-derive whiteouts from the tree itself.
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
	entries       int
	unpackedBytes int64
	diffID        string
	explicitDirs  map[string]struct{}
}

// unpackLayerBlob extracts one compressed layer blob into dest while preserving
// whiteout marker files. It intentionally does not use umoci's layer unpacker:
// umoci consumes whiteouts while this store must retain them for composition.
// Paths are confined to dest.
func unpackCachedLayer(cacheDir string, desc layerDescriptor, dest string) (*unpackStats, error) {
	return unpackCachedLayerContext(context.Background(), cacheDir, desc, dest)
}

func unpackCachedLayerContext(ctx context.Context, cacheDir string, desc layerDescriptor, dest string) (*unpackStats, error) {
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
	stats, err := unpackLayerBlobContext(ctx, blobPath, desc.MediaType, dest)
	if err != nil {
		return nil, fmt.Errorf("unpack layer %s: %w", desc.Digest, err)
	}
	if desc.DiffID != "" && stats.diffID != desc.DiffID {
		return nil, fmt.Errorf("layer %s diff id mismatch: got %s, want %s", desc.Digest, stats.diffID, desc.DiffID)
	}
	return stats, nil
}

func unpackLayerBlob(blobPath, mediaType, dest string) (*unpackStats, error) {
	return unpackLayerBlobContext(context.Background(), blobPath, mediaType, dest)
}

func unpackLayerBlobContext(ctx context.Context, blobPath, mediaType, dest string) (*unpackStats, error) {
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
	reader = contextReader{ctx: ctx, reader: reader}

	hash := sha256.New()
	stats := &unpackStats{explicitDirs: make(map[string]struct{})}
	// Directory metadata is re-applied after extraction, once children
	// exist, so tar directory mtimes are not overwritten by later writes.
	pendingDirs := make([]pendingDir, 0)
	pendingHardlinks := make([]pendingHardlink, 0)
	limitedReader := &io.LimitedReader{R: reader, N: maxLayerUnpackedBytes + 1}
	hashedReader := io.TeeReader(limitedReader, hash)
	tr := tar.NewReader(hashedReader)
	for {
		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read tar entry: %w", err)
		}

		target, err := safeJoin(dest, header.Name)
		if err != nil {
			return nil, err
		}
		if stats.entries >= maxLayerEntries {
			return nil, fmt.Errorf("layer exceeds maximum entry count of %d", maxLayerEntries)
		}
		if header.Size > maxLayerUnpackedBytes-stats.unpackedBytes {
			return nil, fmt.Errorf("layer exceeds maximum unpacked size of %d bytes", maxLayerUnpackedBytes)
		}
		stats.entries++

		base := filepath.Base(header.Name)
		if strings.HasPrefix(base, whiteoutPrefix) && base != opaqueWhiteout {
			targetName := strings.TrimPrefix(base, whiteoutPrefix)
			if targetName == "" || targetName == "." || targetName == ".." {
				return nil, fmt.Errorf("invalid whiteout entry: %s", header.Name)
			}
		}

		if header.Typeflag == tar.TypeDir {
			stats.explicitDirs[filepath.Clean(header.Name)] = struct{}{}
			pendingDirs = append(pendingDirs, pendingDir{target: target, header: header})
		}
		if header.Typeflag == tar.TypeLink {
			// Hardlinks may reference entries that appear later in the tar, so
			// resolve them after extracting all non-link entries.
			pendingHardlinks = append(pendingHardlinks, pendingHardlink{target: target, linkname: header.Linkname})
		} else if err := extractTarEntry(tr, header, dest, target); err != nil {
			return nil, fmt.Errorf("extract %s: %w", header.Name, err)
		}
		if header.Typeflag == tar.TypeReg || header.Typeflag == tar.TypeGNUSparse {
			stats.unpackedBytes += header.Size
		}
	}
	if _, err := io.Copy(io.Discard, hashedReader); err != nil {
		return nil, fmt.Errorf("drain layer: %w", err)
	}
	if limitedReader.N == 0 {
		return nil, fmt.Errorf("layer exceeds maximum unpacked size of %d bytes", maxLayerUnpackedBytes)
	}
	if err := resolveHardlinks(dest, pendingHardlinks); err != nil {
		return nil, err
	}
	for i := len(pendingDirs) - 1; i >= 0; i-- {
		dir := pendingDirs[i]
		if err := applyTarMetadata(dir.target, dir.header); err != nil {
			return nil, fmt.Errorf("restore dir metadata %s: %w", dir.target, err)
		}
	}
	stats.diffID = fmt.Sprintf("sha256:%x", hash.Sum(nil))
	return stats, nil
}

type pendingDir struct {
	target string
	header *tar.Header
}

type pendingHardlink struct {
	target     string
	linkname   string
	linkTarget string
}

func resolveHardlinks(root string, pending []pendingHardlink) error {
	waiting := make(map[string][]pendingHardlink)
	ready := make([]pendingHardlink, 0, len(pending))
	for _, link := range pending {
		linkname := filepath.Clean(link.linkname)
		if filepath.IsAbs(linkname) {
			linkname = strings.TrimPrefix(linkname, string(filepath.Separator))
		}
		linkTarget, err := safeJoin(root, linkname)
		if err != nil {
			return err
		}
		link.linkTarget = linkTarget
		if _, err := os.Lstat(linkTarget); err == nil {
			ready = append(ready, link)
		} else if os.IsNotExist(err) {
			waiting[linkTarget] = append(waiting[linkTarget], link)
		} else {
			return err
		}
	}

	resolved := 0
	for len(ready) > 0 {
		link := ready[0]
		ready = ready[1:]
		if err := prepareTarTarget(link.target); err != nil {
			return err
		}
		if err := os.Link(link.linkTarget, link.target); err != nil {
			return fmt.Errorf("create hardlink %s -> %s: %w", link.target, link.linkname, err)
		}
		resolved++
		ready = append(ready, waiting[link.target]...)
		delete(waiting, link.target)
	}
	if resolved != len(pending) {
		for _, links := range waiting {
			return fmt.Errorf("hardlink target not found for %s", links[0].linkname)
		}
	}
	return nil
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

// safeJoin resolves a tar entry inside root and rejects symlinked parents
// while extracting a layer. This prevents one tar entry from changing where
// a later entry is written.
func safeJoin(root, name string) (string, error) {
	resolved, err := safeJoinForComposition(root, name)
	if err != nil {
		return "", err
	}
	root = filepath.Clean(root)
	clean := filepath.Clean(name)
	target := filepath.Join(root, clean)
	for parent := filepath.Dir(target); parent != root; parent = filepath.Dir(parent) {
		info, err := os.Lstat(parent)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return "", fmt.Errorf("inspect tar entry parent: %w", err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("tar entry traverses symlink: %s", name)
		}
		if !info.IsDir() {
			return "", fmt.Errorf("tar entry parent is not a directory: %s", parent)
		}
	}
	return resolved, nil
}

// safeJoinForComposition resolves an entry through existing parent symlinks,
// interpreting absolute link targets relative to the image root.
func safeJoinForComposition(root, name string) (string, error) {
	root = filepath.Clean(root)
	rootInfo, err := os.Lstat(root)
	if err != nil {
		return "", fmt.Errorf("inspect extraction root: %w", err)
	}
	if !rootInfo.IsDir() || rootInfo.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("extraction root is not a directory: %s", root)
	}
	if filepath.IsAbs(name) {
		return "", fmt.Errorf("tar entry escapes root: %s", name)
	}
	clean := filepath.Clean(name)
	if clean == "." {
		return root, nil
	}
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("tar entry escapes root: %s", name)
	}
	target := filepath.Join(root, clean)
	if !strings.HasPrefix(target, root+string(filepath.Separator)) {
		return "", fmt.Errorf("tar entry escapes root: %s", name)
	}
	parent := filepath.Dir(target)
	resolvedParent, err := resolveLayerPath(root, parent, 0)
	if err != nil {
		return "", fmt.Errorf("inspect tar entry parent: %w", err)
	}
	return filepath.Join(resolvedParent, filepath.Base(target)), nil
}

func resolveLayerPath(root, path string, depth int) (string, error) {
	if depth > 40 {
		return "", fmt.Errorf("too many symlinks")
	}
	root = filepath.Clean(root)
	path = filepath.Clean(path)
	rel, err := filepath.Rel(root, path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path escapes root")
	}
	if rel == "." {
		return root, nil
	}
	parts := strings.Split(rel, string(filepath.Separator))
	current := root
	for i, part := range parts {
		candidate := filepath.Join(current, part)
		info, err := os.Lstat(candidate)
		if err != nil {
			if os.IsNotExist(err) {
				return filepath.Join(candidate, filepath.Join(parts[i+1:]...)), nil
			}
			return "", err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			link, err := os.Readlink(candidate)
			if err != nil {
				return "", err
			}
			var linkTarget string
			if filepath.IsAbs(link) {
				linkTarget = filepath.Join(root, strings.TrimPrefix(link, string(filepath.Separator)))
			} else {
				linkTarget = filepath.Join(filepath.Dir(candidate), link)
			}
			linkTarget = filepath.Clean(linkTarget)
			if !pathWithinRoot(root, linkTarget) {
				return "", fmt.Errorf("symlink target escapes root")
			}
			return resolveLayerPath(root, filepath.Join(linkTarget, filepath.Join(parts[i+1:]...)), depth+1)
		}
		if i < len(parts)-1 && !info.IsDir() {
			return "", fmt.Errorf("path parent is not a directory: %s", candidate)
		}
		current = candidate
	}
	return current, nil
}

func pathWithinRoot(root, path string) bool {
	rel, err := filepath.Rel(filepath.Clean(root), filepath.Clean(path))
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func resolveCompositionDirTarget(root, target string) (string, error) {
	resolved, err := resolveLayerPath(root, target, 0)
	if err == nil {
		return resolved, nil
	}
	info, statErr := os.Lstat(target)
	if statErr == nil && info.Mode()&os.ModeSymlink != 0 {
		if removeErr := removePath(target); removeErr != nil {
			return "", removeErr
		}
		return target, nil
	}
	return "", err
}

// validateSymlinkTarget permits absolute targets because OCI images may use
// them and the image filesystem must preserve their link text. Extraction
// still rejects symlink traversal for later entries, while composition copies
// symlink text without following it.
func validateSymlinkTarget(root, target, linkname string) error {
	if filepath.IsAbs(linkname) {
		return nil
	}
	resolved := filepath.Clean(filepath.Join(filepath.Dir(target), linkname))
	root = filepath.Clean(root)
	if resolved != root && !strings.HasPrefix(resolved, root+string(filepath.Separator)) {
		return fmt.Errorf("symlink target escapes root: %s", linkname)
	}
	return nil
}

func extractTarEntry(tr *tar.Reader, header *tar.Header, root, target string) error {
	switch header.Typeflag {
	case tar.TypeDir:
		return extractTarDir(target)
	case tar.TypeReg, tar.TypeGNUSparse:
		return extractTarFile(tr, target, header)
	case tar.TypeSymlink:
		return extractTarSymlink(root, target, header)
	case tar.TypeChar, tar.TypeBlock:
		return extractTarDevice(target, header)
	case tar.TypeFifo:
		return extractTarFIFO(target, header)
	default:
		return nil
	}
}

func prepareTarTarget(target string) error {
	if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
		return err
	}
	return removePath(target)
}

func extractTarDir(target string) error {
	if info, err := os.Lstat(target); err == nil && !info.IsDir() {
		if err := removePath(target); err != nil {
			return err
		}
	}
	return os.MkdirAll(target, 0755)
}

func extractTarFile(tr *tar.Reader, target string, header *tar.Header) error {
	if err := prepareTarTarget(target); err != nil {
		return err
	}
	file, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC|syscall.O_NOFOLLOW, 0644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(file, tr); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return applyTarMetadata(target, header)
}

func extractTarSymlink(root, target string, header *tar.Header) error {
	if err := validateSymlinkTarget(root, target, header.Linkname); err != nil {
		return err
	}
	if err := prepareTarTarget(target); err != nil {
		return err
	}
	if err := os.Symlink(header.Linkname, target); err != nil {
		return err
	}
	return applyTarMetadata(target, header)
}

func extractTarDevice(target string, header *tar.Header) error {
	if err := prepareTarTarget(target); err != nil {
		return err
	}
	mode := uint32(syscall.S_IFCHR)
	if header.Typeflag == tar.TypeBlock {
		mode = uint32(syscall.S_IFBLK)
	}
	dev := int(unix.Mkdev(uint32(header.Devmajor), uint32(header.Devminor)))
	if err := mknodWithRootlessFallback(target, mode|uint32(header.FileInfo().Mode().Perm()), dev); err != nil {
		return err
	}
	return applyTarMetadata(target, header)
}

func extractTarFIFO(target string, header *tar.Header) error {
	if err := prepareTarTarget(target); err != nil {
		return err
	}
	if err := syscall.Mkfifo(target, uint32(header.FileInfo().Mode().Perm())); err != nil {
		return err
	}
	return applyTarMetadata(target, header)
}

func applyTarMetadata(path string, header *tar.Header) error {
	mtime := header.ModTime
	if mtime.IsZero() {
		mtime = time.Now()
	}
	atime := header.AccessTime
	if atime.IsZero() {
		atime = mtime
	}
	return applyEntryMetadata(path, entryMetadata{
		uid:      header.Uid,
		gid:      header.Gid,
		hasOwner: true,
		mode:     header.FileInfo().Mode(),
		symlink:  header.Typeflag == tar.TypeSymlink,
		atime:    atime,
		mtime:    mtime,
		xattrs:   tarXattrs(header),
	})
}
