package images

import (
	"archive/tar"
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

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

// applyLayerTree merges one unpacked layer directory into targetDir following
// OCI whiteout semantics: whiteouts and opaque markers remove what lower layers
// contributed, then the layer's own entries are copied on top. Raw tar
// whiteout files are interpreted here rather than passed through, because
// overlayfs does not understand them. No production caller yet; composition
// lands in a later change.
//
// explicitDirs, when non-nil, names the directories the layer tar listed
// explicitly (relative, cleaned); only those have their metadata copied.
// A nil map treats every directory as explicit.
func applyLayerTree(layerDir, targetDir string, explicitDirs map[string]struct{}) (err error) {
	if err := os.MkdirAll(targetDir, 0755); err != nil {
		return fmt.Errorf("create target directory: %w", err)
	}
	originalModes := make(map[string]fs.FileMode)
	defer func() {
		if restoreErr := restoreDirectoryModes(originalModes); restoreErr != nil {
			err = errors.Join(err, fmt.Errorf("restore directory modes: %w", restoreErr))
		}
	}()

	// Prepare directories before whiteouts so nested markers are applied after
	// the layer has replaced any conflicting lower-layer entry.
	if err := prepareLayerDirectories(layerDir, targetDir, originalModes); err != nil {
		return fmt.Errorf("prepare layer directories: %w", err)
	}

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
		targetParent, err := safeJoinForComposition(targetDir, filepath.Dir(rel))
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
		if hidden == "" || hidden == "." || hidden == ".." {
			return fmt.Errorf("invalid whiteout entry: %s", rel)
		}
		target, err := safeJoinForComposition(targetDir, filepath.Join(filepath.Dir(rel), hidden))
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
	pendingDirs := make([]dirMeta, 0)
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
		target, err := safeJoinForComposition(targetDir, rel)
		if err != nil {
			return err
		}
		if entry.IsDir() {
			target, err = resolveCompositionDirTarget(targetDir, target)
			if err != nil {
				return err
			}
		}
		if err := makePathWritable(targetDir, filepath.Dir(target), originalModes); err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if _, explicit := explicitDirs[filepath.Clean(rel)]; explicitDirs == nil || explicit {
				pendingDirs = append(pendingDirs, dirMeta{src: path, dst: target, info: info})
			}
		}
		if err := copyEntryInto(path, target, info, hardlinks, targetDir); err != nil {
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
	for i := len(pendingDirs) - 1; i >= 0; i-- {
		dir := pendingDirs[i]
		if err := copyEntryMetadata(dir.src, dir.dst, dir.info); err != nil {
			return fmt.Errorf("restore dir metadata %s: %w", dir.dst, err)
		}
		delete(originalModes, dir.dst)
	}
	return nil
}

func prepareLayerDirectories(layerDir, targetDir string, originalModes map[string]fs.FileMode) error {
	return filepath.WalkDir(layerDir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == layerDir || !entry.IsDir() {
			return nil
		}
		if strings.HasPrefix(entry.Name(), whiteoutPrefix) {
			return filepath.SkipDir
		}
		rel, err := filepath.Rel(layerDir, path)
		if err != nil {
			return err
		}
		target, err := safeJoinForComposition(targetDir, rel)
		if err != nil {
			return err
		}
		target, err = resolveCompositionDirTarget(targetDir, target)
		if err != nil {
			return err
		}
		if err := makePathWritable(targetDir, filepath.Dir(target), originalModes); err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if err := copyDirectoryEntry(target, info); err != nil {
			return err
		}
		return makePathWritable(targetDir, target, originalModes)
	})
}

func makePathWritable(root, path string, originalModes map[string]fs.FileMode) error {
	root = filepath.Clean(root)
	path = filepath.Clean(path)
	if !pathWithinRoot(root, path) {
		return fmt.Errorf("path is outside target root: %s", path)
	}
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return err
	}
	if err := makeDirWritable(root, originalModes); err != nil {
		return err
	}
	if rel == "." {
		return nil
	}
	current := root
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		current = filepath.Join(current, part)
		if err := makeDirWritable(current, originalModes); err != nil {
			return err
		}
	}
	return nil
}

func makeDirWritable(dir string, originalModes map[string]fs.FileMode) error {
	info, err := os.Lstat(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("target parent is not a directory: %s", dir)
	}
	if _, recorded := originalModes[dir]; !recorded {
		originalModes[dir] = info.Mode()
		if err := os.Chmod(dir, info.Mode().Perm()|0700); err != nil {
			return err
		}
	}
	return nil
}

func restoreDirectoryModes(originalModes map[string]fs.FileMode) error {
	paths := make([]string, 0, len(originalModes))
	for path := range originalModes {
		paths = append(paths, path)
	}
	sort.Slice(paths, func(i, j int) bool {
		return len(paths[i]) > len(paths[j])
	})
	var restoreErr error
	for _, path := range paths {
		mode := originalModes[path]
		info, err := os.Lstat(path)
		if os.IsNotExist(err) || (err == nil && (info.Mode()&os.ModeSymlink != 0 || !info.IsDir())) {
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
		if err := removePath(filepath.Join(dir, entry.Name())); err != nil {
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
func copyEntryInto(src, dst string, info os.FileInfo, hardlinks map[hardlinkIdentity]string, root string) error {
	switch info.Mode() & fs.ModeType {
	case 0:
		return copyRegularEntry(src, dst, info, hardlinks)
	case fs.ModeDir:
		return copyDirectoryEntry(dst, info)
	case fs.ModeSymlink:
		return copySymlinkEntry(src, dst, info, root)
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
	if err := copyFileContents(src, dst, info); err != nil {
		return err
	}
	return copyEntryMetadata(src, dst, info)
}

// copyDirectoryEntry creates dst as a directory, replacing a conflicting
// non-directory. Callers resolve dst through resolveCompositionDirTarget
// first, so dst is never an existing symlink here.
func copyDirectoryEntry(dst string, info os.FileInfo) error {
	if existing, err := os.Lstat(dst); err == nil && !existing.IsDir() {
		if err := removePath(dst); err != nil {
			return err
		}
	}
	return os.MkdirAll(dst, info.Mode().Perm())
}

func copySymlinkEntry(src, dst string, info os.FileInfo, root string) error {
	linkTarget, err := os.Readlink(src)
	if err != nil {
		return err
	}
	if err := validateSymlinkTarget(root, dst, linkTarget); err != nil {
		return err
	}
	if err := removePath(dst); err != nil {
		return err
	}
	if err := os.Symlink(linkTarget, dst); err != nil {
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
	if err := mknodWithRootlessFallback(dst, mode|uint32(info.Mode().Perm()), int(stat.Rdev)); err != nil {
		return err
	}
	return copyEntryMetadata(src, dst, info)
}

func specialFileMode(mode fs.FileMode) (uint32, error) {
	switch {
	case mode&fs.ModeCharDevice != 0:
		return syscall.S_IFCHR, nil
	case mode&fs.ModeDevice != 0:
		return syscall.S_IFBLK, nil
	case mode&fs.ModeNamedPipe != 0:
		return syscall.S_IFIFO, nil
	case mode&fs.ModeSocket != 0:
		return syscall.S_IFSOCK, nil
	default:
		return 0, fmt.Errorf("unsupported file mode")
	}
}

type entryMetadata struct {
	uid, gid int
	hasOwner bool
	mode     fs.FileMode
	symlink  bool
	atime    time.Time
	mtime    time.Time
	xattrs   map[string][]byte
}

func applyEntryMetadata(path string, metadata entryMetadata) error {
	if metadata.hasOwner {
		if err := os.Lchown(path, metadata.uid, metadata.gid); err != nil && !errors.Is(err, os.ErrPermission) && !errors.Is(err, unix.EPERM) {
			return err
		}
	}
	if metadata.symlink {
		return nil
	}
	if err := applyXattrs(path, metadata.xattrs); err != nil {
		return err
	}
	mode := metadata.mode.Perm() | metadata.mode&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky)
	if err := os.Chmod(path, mode); err != nil {
		return err
	}
	return os.Chtimes(path, metadata.atime, metadata.mtime)
}

func copyEntryMetadata(src, dst string, info os.FileInfo) error {
	metadata := entryMetadata{
		mode:    info.Mode(),
		symlink: info.Mode()&os.ModeSymlink != 0,
		atime:   info.ModTime(),
		mtime:   info.ModTime(),
	}
	if stat, ok := info.Sys().(*syscall.Stat_t); ok {
		metadata.uid = int(stat.Uid)
		metadata.gid = int(stat.Gid)
		metadata.hasOwner = true
		metadata.atime = statAtime(stat)
	}
	if !metadata.symlink {
		xattrs, err := readXattrs(src)
		if err != nil {
			return err
		}
		metadata.xattrs = xattrs
	}
	return applyEntryMetadata(dst, metadata)
}

func readXattrs(src string) (map[string][]byte, error) {
	size, err := unix.Llistxattr(src, nil)
	if err != nil {
		if errors.Is(err, unix.ENOTSUP) || errors.Is(err, unix.EPERM) {
			return nil, nil
		}
		return nil, err
	}
	names := make([]byte, size)
	if size > 0 {
		n, err := unix.Llistxattr(src, names)
		if err != nil {
			return nil, err
		}
		names = names[:n]
	}
	xattrs := make(map[string][]byte)
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
			return nil, err
		}
		value := make([]byte, size)
		n, err := unix.Lgetxattr(src, name, value)
		if err != nil {
			return nil, err
		}
		xattrs[name] = value[:n]
	}
	return xattrs, nil
}

func tarXattrs(header *tar.Header) map[string][]byte {
	const paxXattrPrefix = "SCHILY.xattr."
	xattrs := make(map[string][]byte, len(header.Xattrs))
	for name, value := range header.Xattrs {
		xattrs[name] = []byte(value)
	}
	for key, value := range header.PAXRecords {
		if strings.HasPrefix(key, paxXattrPrefix) {
			name := strings.TrimPrefix(key, paxXattrPrefix)
			if _, exists := xattrs[name]; !exists {
				xattrs[name] = []byte(value)
			}
		}
	}
	return xattrs
}

func applyXattrs(path string, xattrs map[string][]byte) error {
	for name, value := range xattrs {
		if err := unix.Lsetxattr(path, name, value, 0); err != nil && !errors.Is(err, unix.ENOTSUP) && !errors.Is(err, unix.EPERM) {
			return fmt.Errorf("restore xattr %s: %w", name, err)
		}
	}
	return nil
}

func copyFileContents(src, dst string, info os.FileInfo) (err error) {
	sourceMode := info.Mode().Perm() | info.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky)
	if sourceMode&0400 == 0 {
		if err := os.Chmod(src, sourceMode|0400); err != nil {
			return err
		}
		defer func() {
			if restoreErr := os.Chmod(src, sourceMode); err == nil {
				err = restoreErr
			}
		}()
	}

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
