package images

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
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

// layerArtifact is the persisted record for one materialized layer artifact.
// The key is the compressed layer blob digest plus the artifact format, so the
// same layer can coexist in several materializations.
type layerArtifact struct {
	SchemaVersion int              `json:"schema_version"`
	Digest        string           `json:"digest"` // compressed layer blob digest, sha256:...
	DiffID        string           `json:"diff_id,omitempty"`
	Format        string           `json:"format"`
	SizeBytes     int64            `json:"size_bytes"` // artifact bytes on disk
	UnpackedBytes int64            `json:"unpacked_bytes"`
	Entries       int              `json:"entries"`
	Whiteouts     []whiteoutRecord `json:"whiteouts,omitempty"`
	CreatedAt     time.Time        `json:"created_at"`
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
	if a.Format != layerFormatErofs && a.Format != layerFormatExt4 {
		return fmt.Errorf("invalid format: %s", a.Format)
	}
	if a.SizeBytes < 0 || a.UnpackedBytes < 0 || a.Entries < 0 {
		return fmt.Errorf("invalid size or entry counts")
	}
	return nil
}

// whiteoutRecord describes one whiteout marker found in a layer. Dir is the
// directory containing the marker relative to the layer root ("" for root).
// For opaque markers, Target is empty and the whole directory is hidden.
type whiteoutRecord struct {
	Dir    string `json:"dir"`
	Target string `json:"target,omitempty"`
	Opaque bool   `json:"opaque,omitempty"`
}

func (a *layerArtifact) matches(desc layerDescriptor) bool {
	if a.Digest != desc.Digest || a.Format != layerArtifactFormat() {
		return false
	}
	return desc.DiffID == "" || a.DiffID == desc.DiffID
}

const (
	layerFormatErofs = "erofs"
	layerFormatExt4  = "ext4"
)

func layerArtifactFormat() string {
	switch DefaultImageFormat {
	case FormatErofs:
		return layerFormatErofs
	case FormatExt4:
		return layerFormatExt4
	default:
		return ""
	}
}

func layerArtifactPath(p *paths.Paths, layerHex string) string {
	return p.ImageLayerArtifactForFormat(layerHex, layerArtifactFormat())
}

// readLayerRecord loads the artifact record for a layer digest, if present.
// A missing record returns (nil, nil): the layer simply was never
// materialized.
func readLayerRecord(p *paths.Paths, layerHex string) (*layerArtifact, error) {
	data, err := os.ReadFile(p.ImageLayerRecord(layerHex))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read layer record: %w", err)
	}
	var record layerArtifact
	if err := json.Unmarshal(data, &record); err != nil {
		return nil, fmt.Errorf("unmarshal layer record: %w", err)
	}
	if err := record.validate(); err != nil {
		return nil, fmt.Errorf("invalid layer record: %w", err)
	}
	return &record, nil
}

// materializeLayerArtifact ensures a layer has a materialized artifact keyed
// by its blob digest, building it from the shared OCI cache blob when absent.
// The layer is unpacked into an isolated temp directory, converted to the
// default image format, and installed atomically; an interrupted build leaves
// only temp files that the next attempt replaces. No production caller yet:
// pull integration and composition land in later changes.
func (m *manager) materializeLayerArtifact(desc layerDescriptor) (*layerArtifact, error) {
	unlock := m.layerLocks.lock(desc.Digest)
	defer unlock()

	if layerArtifactFormat() == "" {
		return nil, fmt.Errorf("unsupported layer artifact format: %s", DefaultImageFormat)
	}
	layerHex := strings.TrimPrefix(desc.Digest, "sha256:")
	if err := paths.ValidatePathComponent(layerHex); err != nil {
		return nil, fmt.Errorf("invalid layer digest: %s", desc.Digest)
	}

	if record, err := readLayerRecord(m.paths, layerHex); err != nil {
		return nil, err
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
	defer os.RemoveAll(unpackDir)

	stats, err := unpackLayerBlob(blobPath, desc.MediaType, unpackDir)
	if err != nil {
		return nil, fmt.Errorf("unpack layer %s: %w", desc.Digest, err)
	}
	if desc.DiffID != "" && stats.diffID != desc.DiffID {
		return nil, fmt.Errorf("layer %s diff id mismatch: got %s, want %s", desc.Digest, stats.diffID, desc.DiffID)
	}

	return m.installLayerArtifact(desc, layerHex, unpackDir, stats)
}

func (m *manager) installLayerArtifact(desc layerDescriptor, layerHex, unpackDir string, stats *unpackStats) (*layerArtifact, error) {
	record := &layerArtifact{
		SchemaVersion: layerRecordSchemaVersion,
		Digest:        desc.Digest,
		DiffID:        desc.DiffID,
		Format:        layerArtifactFormat(),
		UnpackedBytes: stats.unpackedBytes,
		Entries:       stats.entries,
		// Nothing reads Whiteouts yet; composition re-derives whiteouts
		// from the unpacked tree once it lands.
		Whiteouts: stats.whiteouts,
		CreatedAt: time.Now(),
	}

	if err := installAtomically(layerArtifactPath(m.paths, layerHex), func(path string) error {
		// The artifact intentionally retains .wh. marker files so
		// composition can re-derive whiteouts from the tree itself.
		size, convErr := ExportRootfs(unpackDir, path, DefaultImageFormat)
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
	if err := writeJSONAtomic(m.paths.ImageLayerRecord(layerHex), data); err != nil {
		_ = os.Remove(layerArtifactPath(m.paths, layerHex))
		return nil, fmt.Errorf("write layer record: %w", err)
	}
	return record, nil
}

type unpackStats struct {
	entries       int
	unpackedBytes int64
	diffID        string
	whiteouts     []whiteoutRecord
}

// unpackLayerBlob extracts one compressed layer blob into dest, preserving
// whiteout marker files and recording them. Paths are confined to dest.
func unpackLayerBlob(blobPath, mediaType, dest string) (*unpackStats, error) {
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
	stats := &unpackStats{whiteouts: make([]whiteoutRecord, 0)}
	// Directory metadata is re-applied after extraction, once children
	// exist, so tar directory mtimes are not overwritten by later writes.
	var pendingDirs []pendingDir
	var pendingHardlinks []pendingHardlink
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

		dir, base := filepath.Dir(header.Name), filepath.Base(header.Name)
		if dir == "." {
			dir = ""
		}
		if base == opaqueWhiteout {
			stats.whiteouts = append(stats.whiteouts, whiteoutRecord{Dir: dir, Opaque: true})
		} else if strings.HasPrefix(base, whiteoutPrefix) {
			targetName := strings.TrimPrefix(base, whiteoutPrefix)
			if targetName == "" || targetName == "." || targetName == ".." {
				return nil, fmt.Errorf("invalid whiteout entry: %s", header.Name)
			}
			stats.whiteouts = append(stats.whiteouts, whiteoutRecord{Dir: dir, Target: targetName})
		}

		if header.Typeflag == tar.TypeDir {
			pendingDirs = append(pendingDirs, pendingDir{target: target, header: header})
		}
		if header.Typeflag == tar.TypeLink {
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
	for _, dir := range pendingDirs {
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
	target   string
	linkname string
}

func resolveHardlinks(root string, pending []pendingHardlink) error {
	for len(pending) > 0 {
		resolved := 0
		remaining := make([]pendingHardlink, 0, len(pending))
		for _, link := range pending {
			linkTarget, err := safeJoin(root, link.linkname)
			if err != nil {
				return err
			}
			if _, err := os.Lstat(linkTarget); err != nil {
				if os.IsNotExist(err) {
					remaining = append(remaining, link)
					continue
				}
				return err
			}
			if err := prepareTarTarget(link.target); err != nil {
				return err
			}
			if err := os.Link(linkTarget, link.target); err != nil {
				return fmt.Errorf("create hardlink %s: %w", link.target, err)
			}
			resolved++
		}
		if resolved == 0 {
			return fmt.Errorf("hardlink target not found")
		}
		pending = remaining
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

// safeJoin resolves a tar entry name inside root and rejects symlinked
// parents: extraction must never create an entry through a symlink an earlier
// tar entry planted, so existing parents are Lstat-walked and rejected rather
// than resolved. Symlink entries themselves may legitimately name paths that
// do not exist yet, so their targets are checked by resolve in
// validateSymlinkTarget instead.
func safeJoin(root, name string) (string, error) {
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
		return filepath.Clean(root), nil
	}
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("tar entry escapes root: %s", name)
	}
	root = filepath.Clean(root)
	target := filepath.Join(root, clean)
	if target == root {
		return target, nil
	}
	if !strings.HasPrefix(target, root+string(filepath.Separator)) {
		return "", fmt.Errorf("tar entry escapes root: %s", name)
	}
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
	return target, nil
}

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
	case tar.TypeLink:
		return extractTarHardlink(root, target, header)
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

func extractTarHardlink(root, target string, header *tar.Header) error {
	linkTarget, err := safeJoin(root, header.Linkname)
	if err != nil {
		return err
	}
	if err := prepareTarTarget(target); err != nil {
		return err
	}
	return os.Link(linkTarget, target)
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
	if err := unix.Mknod(target, mode|uint32(header.FileInfo().Mode().Perm()), dev); err != nil {
		if !errors.Is(err, unix.EPERM) {
			return fmt.Errorf("mknod: %w", err)
		}
		file, openErr := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|syscall.O_NOFOLLOW, 0644)
		if openErr != nil {
			return fmt.Errorf("create rootless device placeholder: %w", openErr)
		}
		if closeErr := file.Close(); closeErr != nil {
			return closeErr
		}
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
	if err := os.Lchown(path, header.Uid, header.Gid); err != nil && !errors.Is(err, os.ErrPermission) && !errors.Is(err, unix.EPERM) {
		return fmt.Errorf("restore ownership: %w", err)
	}
	if header.Typeflag != tar.TypeSymlink {
		if err := os.Chmod(path, os.FileMode(header.Mode)); err != nil {
			return fmt.Errorf("restore mode: %w", err)
		}
		mtime := header.ModTime
		if mtime.IsZero() {
			mtime = time.Now()
		}
		atime := header.AccessTime
		if atime.IsZero() {
			atime = mtime
		}
		if err := os.Chtimes(path, atime, mtime); err != nil {
			return fmt.Errorf("restore timestamps: %w", err)
		}
	}
	for name, value := range header.Xattrs {
		if err := unix.Lsetxattr(path, name, []byte(value), 0); err != nil && !errors.Is(err, unix.ENOTSUP) && !errors.Is(err, unix.EPERM) {
			return fmt.Errorf("restore xattr %s: %w", name, err)
		}
	}
	return nil
}

// removePath removes whatever entry occupies path, including non-empty
// directories, and tolerates a missing path.
func removePath(path string) error {
	if err := os.RemoveAll(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// applyLayerTree merges one unpacked layer directory into targetDir following
// OCI whiteout semantics: whiteouts and opaque markers remove what lower layers
// contributed, then the layer's own entries are copied on top. Raw tar
// whiteout files are interpreted here rather than passed through, because
// overlayfs does not understand them. No production caller yet; composition
// lands in a later change.
func applyLayerTree(layerDir, targetDir string) (err error) {
	if err := os.MkdirAll(targetDir, 0755); err != nil {
		return fmt.Errorf("create target directory: %w", err)
	}
	originalModes := make(map[string]fs.FileMode)
	defer func() {
		if restoreErr := restoreDirectoryModes(originalModes); restoreErr != nil {
			if err == nil {
				err = fmt.Errorf("restore directory modes: %w", restoreErr)
			} else {
				err = errors.Join(err, fmt.Errorf("restore directory modes: %w", restoreErr))
			}
		}
	}()

	// Phase 1: apply whiteouts against what is already in the target.
	err = filepath.WalkDir(layerDir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		base := entry.Name()
		if path == layerDir || !strings.HasPrefix(base, whiteoutPrefix) {
			return nil
		}
		rel, err := filepath.Rel(layerDir, path)
		if err != nil {
			return err
		}
		targetParent, err := safeJoin(targetDir, filepath.Dir(rel))
		if err != nil {
			return err
		}
		if err := makePathWritable(targetDir, targetParent, originalModes); err != nil {
			return err
		}
		if base == opaqueWhiteout {
			return clearDirContents(targetParent)
		}
		hidden := strings.TrimPrefix(base, whiteoutPrefix)
		target, err := safeJoin(targetDir, filepath.Join(filepath.Dir(rel), hidden))
		if err != nil {
			return err
		}
		return removePath(target)
	})
	if err != nil {
		return fmt.Errorf("apply whiteouts: %w", err)
	}

	// Phase 2: copy the layer's own entries, skipping whiteout markers.
	// Directory metadata is deferred until all children are copied, so tar
	// directory mtimes survive the merge.
	var pendingDirs []dirMeta
	hardlinks := make(map[hardlinkIdentity]string)
	err = filepath.WalkDir(layerDir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == layerDir {
			return nil
		}
		if strings.HasPrefix(entry.Name(), whiteoutPrefix) {
			if entry.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		rel, err := filepath.Rel(layerDir, path)
		if err != nil {
			return err
		}
		target, err := safeJoin(targetDir, rel)
		if err != nil {
			return err
		}
		if err := makePathWritable(targetDir, filepath.Dir(target), originalModes); err != nil {
			return err
		}
		if entry.IsDir() {
			info, err := entry.Info()
			if err != nil {
				return err
			}
			pendingDirs = append(pendingDirs, dirMeta{src: path, dst: target, info: info})
		}
		if err := copyEntryInto(path, target, hardlinks); err != nil {
			return err
		}
		if entry.IsDir() {
			return makePathWritable(targetDir, target, originalModes)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("copy layer tree: %w", err)
	}
	for _, dir := range pendingDirs {
		if err := copyEntryMetadata(dir.src, dir.dst, dir.info); err != nil {
			return fmt.Errorf("restore dir metadata %s: %w", dir.dst, err)
		}
	}
	return nil
}

func makePathWritable(root, path string, originalModes map[string]fs.FileMode) error {
	root = filepath.Clean(root)
	path = filepath.Clean(path)
	if path != root && !strings.HasPrefix(path, root+string(filepath.Separator)) {
		return fmt.Errorf("path is outside target root: %s", path)
	}
	for current := path; ; current = filepath.Dir(current) {
		info, err := os.Lstat(current)
		if err == nil {
			if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
				return fmt.Errorf("target parent is not a directory: %s", current)
			}
			if _, recorded := originalModes[current]; !recorded {
				originalModes[current] = info.Mode()
				if err := os.Chmod(current, info.Mode().Perm()|0700); err != nil {
					return err
				}
			}
		} else if !os.IsNotExist(err) {
			return err
		}
		if current == root {
			return nil
		}
	}
}

func restoreDirectoryModes(originalModes map[string]fs.FileMode) error {
	var restoreErr error
	for path, mode := range originalModes {
		if _, err := os.Lstat(path); os.IsNotExist(err) {
			continue
		} else if err != nil {
			restoreErr = errors.Join(restoreErr, err)
			continue
		}
		if err := os.Chmod(path, mode.Perm()|mode&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky)); err != nil {
			restoreErr = errors.Join(restoreErr, err)
		}
	}
	return restoreErr
}

type dirMeta struct {
	src, dst string
	info     os.FileInfo
}

// clearDirContents removes everything inside dir without removing dir itself,
// and without following symlinks.
func clearDirContents(dir string) error {
	info, err := os.Lstat(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if !info.IsDir() {
		return removePath(dir)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if err := os.RemoveAll(filepath.Join(dir, entry.Name())); err != nil {
			return err
		}
	}
	return nil
}

type hardlinkIdentity struct {
	dev uint64
	ino uint64
}

// copyEntryInto copies one filesystem entry from src to dst, replacing any
// conflicting entry and preserving hardlinks within the layer.
func copyEntryInto(src, dst string, hardlinks map[hardlinkIdentity]string) error {
	info, err := os.Lstat(src)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
		return err
	}

	switch info.Mode() & fs.ModeType {
	case 0:
		return copyRegularEntry(src, dst, info, hardlinks)
	case fs.ModeDir:
		return copyDirectoryEntry(src, dst, info)
	case fs.ModeSymlink:
		return copySymlinkEntry(src, dst)
	default:
		return copySpecialEntry(src, dst, info)
	}
}

func copyRegularEntry(src, dst string, info os.FileInfo, hardlinks map[hardlinkIdentity]string) error {
	if err := removePath(dst); err != nil {
		return err
	}
	if stat, ok := info.Sys().(*syscall.Stat_t); ok && stat.Nlink > 1 {
		identity := hardlinkIdentity{dev: uint64(stat.Dev), ino: uint64(stat.Ino)}
		if first, seen := hardlinks[identity]; seen {
			return os.Link(first, dst)
		}
		hardlinks[identity] = dst
	}
	if err := copyFileContents(src, dst); err != nil {
		return err
	}
	return copyEntryMetadata(src, dst, info)
}

func copyDirectoryEntry(src, dst string, info os.FileInfo) error {
	if existing, err := os.Lstat(dst); err == nil && !existing.IsDir() {
		if err := removePath(dst); err != nil {
			return err
		}
	}
	return os.MkdirAll(dst, info.Mode().Perm())
}

func copySymlinkEntry(src, dst string) error {
	linkTarget, err := os.Readlink(src)
	if err != nil {
		return err
	}
	if err := removePath(dst); err != nil {
		return err
	}
	if err := os.Symlink(linkTarget, dst); err != nil {
		return err
	}
	info, err := os.Lstat(src)
	if err != nil {
		return err
	}
	return copyEntryMetadata(src, dst, info)
}

func copySpecialEntry(src, dst string, info os.FileInfo) error {
	if err := removePath(dst); err != nil {
		return err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("unsupported entry type for %s", src)
	}
	mode, err := specialFileMode(info.Mode() & fs.ModeType)
	if err != nil {
		return fmt.Errorf("unsupported entry type for %s: %w", src, err)
	}
	if err := unix.Mknod(dst, mode|uint32(info.Mode().Perm()), int(stat.Rdev)); err != nil {
		if !errors.Is(err, unix.EPERM) {
			return fmt.Errorf("mknod: %w", err)
		}
		file, openErr := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|syscall.O_NOFOLLOW, 0644)
		if openErr != nil {
			return fmt.Errorf("create rootless device placeholder: %w", openErr)
		}
		if closeErr := file.Close(); closeErr != nil {
			return closeErr
		}
	}
	return copyEntryMetadata(src, dst, info)
}

func specialFileMode(mode fs.FileMode) (uint32, error) {
	switch mode {
	case fs.ModeCharDevice:
		return syscall.S_IFCHR, nil
	case fs.ModeDevice:
		return syscall.S_IFBLK, nil
	case fs.ModeNamedPipe:
		return syscall.S_IFIFO, nil
	default:
		return 0, fmt.Errorf("unsupported file mode")
	}
}

func copyEntryMetadata(src, dst string, info os.FileInfo) error {
	if stat, ok := info.Sys().(*syscall.Stat_t); ok {
		if err := os.Lchown(dst, int(stat.Uid), int(stat.Gid)); err != nil && !errors.Is(err, os.ErrPermission) && !errors.Is(err, unix.EPERM) {
			return err
		}
	}
	if info.Mode()&os.ModeSymlink == 0 {
		mode := info.Mode().Perm() | info.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky)
		if err := os.Chmod(dst, mode); err != nil {
			return err
		}
		if err := os.Chtimes(dst, info.ModTime(), info.ModTime()); err != nil {
			return err
		}
		if err := copyXattrs(src, dst); err != nil {
			return err
		}
	}
	return nil
}

func copyXattrs(src, dst string) error {
	size, err := unix.Llistxattr(src, nil)
	if err != nil {
		if errors.Is(err, unix.ENOTSUP) || errors.Is(err, unix.EPERM) {
			return nil
		}
		return err
	}
	names := make([]byte, size)
	if size > 0 {
		n, err := unix.Llistxattr(src, names)
		if err != nil {
			return err
		}
		names = names[:n]
	}
	for _, attr := range bytes.Split(bytes.TrimSuffix(names, []byte{0}), []byte{0}) {
		if len(attr) == 0 {
			continue
		}
		name := string(attr)
		size, err := unix.Lgetxattr(src, name, nil)
		if err != nil {
			if errors.Is(err, unix.ENOTSUP) || errors.Is(err, unix.EPERM) || errors.Is(err, unix.ENODATA) {
				continue
			}
			return err
		}
		value := make([]byte, size)
		if _, err := unix.Lgetxattr(src, name, value); err != nil {
			return err
		}
		if err := unix.Lsetxattr(dst, name, value, 0); err != nil && !errors.Is(err, unix.ENOTSUP) && !errors.Is(err, unix.EPERM) {
			return err
		}
	}
	return nil
}

func copyFileContents(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC|syscall.O_NOFOLLOW, 0644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
