package images

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/kernel/hypeman/lib/paths"
	"github.com/kernel/hypeman/lib/tags"
)

type imageMetadata struct {
	Name         string              `json:"name"`   // Normalized ref (tag or digest)
	Digest       string              `json:"digest"` // Always present: sha256:...
	Platform     string              `json:"platform,omitempty"`
	Status       string              `json:"status"`
	Error        *string             `json:"error,omitempty"`
	Request      *CreateImageRequest `json:"request,omitempty"`
	SizeBytes    int64               `json:"size_bytes"`
	Entrypoint   []string            `json:"entrypoint,omitempty"`
	Cmd          []string            `json:"cmd,omitempty"`
	Env          map[string]string   `json:"env,omitempty"`
	Labels       map[string]string   `json:"labels,omitempty"`
	Tags         tags.Tags           `json:"tags,omitempty"`
	WorkingDir   string              `json:"working_dir,omitempty"`
	CreatedAt    time.Time           `json:"created_at"`
	BorrowedAuth bool                `json:"borrowed_auth,omitempty"`
	BuildID      string              `json:"build_id,omitempty"`
}

func (m *imageMetadata) toImage() *Image {
	platform := m.Platform
	if platform == "" {
		// Images predating platform tracking were pulled at the host platform.
		platform = hostPlatform().String()
	}
	img := &Image{
		Name:      m.Name,
		Digest:    m.Digest,
		Platform:  platform,
		Status:    m.Status,
		Error:     m.Error,
		CreatedAt: m.CreatedAt,
	}

	if m.Status == StatusReady && m.SizeBytes > 0 {
		sizeBytes := m.SizeBytes
		img.SizeBytes = &sizeBytes
	}

	if len(m.Entrypoint) > 0 {
		img.Entrypoint = m.Entrypoint
	}
	if len(m.Cmd) > 0 {
		img.Cmd = m.Cmd
	}
	if len(m.Env) > 0 {
		img.Env = m.Env
	}
	if len(m.Labels) > 0 {
		img.Labels = make(map[string]string, len(m.Labels))
		for key, value := range m.Labels {
			img.Labels[key] = value
		}
	}
	if len(m.Tags) > 0 {
		img.Tags = tags.Clone(m.Tags)
	}
	if m.WorkingDir != "" {
		img.WorkingDir = m.WorkingDir
	}

	return img
}

// legacyDigestDir returns the directory used by the original image layout.
func legacyDigestDir(p *paths.Paths, repository, digestHex string) string {
	return p.ImageDigestDir(repository, digestHex)
}

// contentDigestDir returns the directory used by the content-addressed layout.
func contentDigestDir(p *paths.Paths, digestHex string) string {
	return p.ImageContentDir(digestHex)
}

// imageLayout contains the paths for one complete metadata/rootfs pair.
type imageLayout struct {
	dir      string
	metadata string
	disk     string
	content  bool
}

// resolveImageLayout selects one layout for all operations on a digest. A
// complete legacy image remains authoritative while content is incomplete;
// once content metadata is ready, content becomes canonical even if its disk
// is missing so callers report the corruption instead of mixing layouts.
func resolveImageLayout(p *paths.Paths, repository, digestHex string) imageLayout {
	legacy := imageLayout{
		dir:      legacyDigestDir(p, repository, digestHex),
		metadata: p.ImageMetadata(repository, digestHex),
		disk:     p.ImageDigestPath(repository, digestHex),
	}
	content := imageLayout{
		dir:      contentDigestDir(p, digestHex),
		metadata: p.ImageContentMetadata(digestHex),
		disk:     p.ImageContentPath(digestHex),
		content:  true,
	}

	if legacyImageExists(p, repository, digestHex) {
		contentStatus, contentOK := metadataStatus(content.metadata)
		if !contentOK || contentStatus != StatusReady {
			return legacy
		}
	}
	if pathExists(content.metadata) || pathExists(content.disk) {
		return content
	}
	return legacy
}

func pathExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// digestDir returns the directory for a specific digest, using the same layout
// selection as metadata and disk lookup.
func digestDir(p *paths.Paths, repository, digestHex string) string {
	return resolveImageLayout(p, repository, digestHex).dir
}

// contentDigestPath returns the rootfs path for a newly written image.
func contentDigestPath(p *paths.Paths, digestHex string) string {
	return p.ImageContentPath(digestHex)
}

func legacyImageExists(p *paths.Paths, repository, digestHex string) bool {
	_, metadataErr := os.Stat(p.ImageMetadata(repository, digestHex))
	_, diskErr := os.Stat(p.ImageDigestPath(repository, digestHex))
	return metadataErr == nil && diskErr == nil
}

func metadataStatus(path string) (string, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	var meta imageMetadata
	if err := json.Unmarshal(data, &meta); err != nil {
		return "", false
	}
	return meta.Status, true
}

// digestPath returns the rootfs path for a digest using the selected layout.
func digestPath(p *paths.Paths, repository, digestHex string) string {
	return resolveImageLayout(p, repository, digestHex).disk
}

// GetDiskPath returns the filesystem path to an image's rootfs disk file (public for instances manager)
func GetDiskPath(p *paths.Paths, imageName string, digest string) (string, error) {
	// Parse image name to get repository
	ref, err := ParseNormalizedRef(imageName)
	if err != nil {
		return "", fmt.Errorf("parse image name: %w", err)
	}

	// Extract digest hex (remove "sha256:" prefix)
	digestHex := strings.TrimPrefix(digest, "sha256:")

	return digestPath(p, ref.Repository(), digestHex), nil
}

// metadataPath returns the path to metadata.json for a digest using the
// selected layout.
func metadataPath(p *paths.Paths, repository, digestHex string) string {
	return resolveImageLayout(p, repository, digestHex).metadata
}

// newTagSymlinkPath returns the path to a tag in the repository namespace.
func newTagSymlinkPath(p *paths.Paths, repository, tag string) string {
	return p.ImageRepositoryTagSymlink(repository, tag)
}

// tagSymlinkPath returns the path to a tag symlink in the active layout.
func tagSymlinkPath(p *paths.Paths, repository, tag string) string {
	newPath := newTagSymlinkPath(p, repository, tag)
	if _, err := os.Lstat(newPath); err == nil {
		return newPath
	}
	return p.ImageTagSymlink(repository, tag)
}

func usesLegacyLayout(p *paths.Paths, repository, digestHex string) bool {
	_, legacyErr := os.Stat(p.ImageMetadata(repository, digestHex))
	_, contentErr := os.Stat(p.ImageContentMetadata(digestHex))
	return legacyErr == nil && errors.Is(contentErr, os.ErrNotExist)
}

func contentLayoutForWrite(p *paths.Paths, repository, digestHex string) bool {
	return !usesLegacyLayout(p, repository, digestHex)
}

// writeMetadata writes metadata for a digest.
func writeMetadata(p *paths.Paths, repository, digestHex string, meta *imageMetadata) error {
	path := p.ImageMetadata(repository, digestHex)
	if contentLayoutForWrite(p, repository, digestHex) {
		path = p.ImageContentMetadata(digestHex)
	}
	return writeMetadataFile(path, meta)
}

func writeMetadataFile(path string, meta *imageMetadata) error {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return fmt.Errorf("create metadata directory: %w", err)
	}

	data, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal metadata: %w", err)
	}

	tempPath := path + ".tmp"
	if err := os.WriteFile(tempPath, data, 0644); err != nil {
		return fmt.Errorf("write temp metadata: %w", err)
	}
	if err := os.Rename(tempPath, path); err != nil {
		_ = os.Remove(tempPath)
		return fmt.Errorf("rename metadata: %w", err)
	}
	return nil
}

// readMetadata reads metadata for a digest
func readMetadata(p *paths.Paths, repository, digestHex string) (*imageMetadata, error) {
	path := metadataPath(p, repository, digestHex)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("read metadata: %w", err)
	}

	var meta imageMetadata
	if err := json.Unmarshal(data, &meta); err != nil {
		return nil, fmt.Errorf("unmarshal metadata: %w", err)
	}

	if meta.Status == StatusReady {
		diskPath := digestPath(p, repository, digestHex)
		if _, err := os.Stat(diskPath); err != nil {
			if os.IsNotExist(err) {
				return nil, fmt.Errorf("disk image missing: %s", diskPath)
			}
			return nil, fmt.Errorf("stat disk image: %w", err)
		}
	}

	return &meta, nil
}

func promoteImageToContent(p *paths.Paths, sourceRepository, digestHex string, sourceMeta *imageMetadata) error {
	contentDir := contentDigestDir(p, digestHex)
	contentReady := false
	if contentMeta, err := readMetadata(p, "", digestHex); err == nil {
		contentReady = contentMeta.Status == StatusReady
	}
	if !contentReady {
		sourceDiskPath := digestPath(p, sourceRepository, digestHex)
		if _, err := os.Stat(sourceDiskPath); err != nil {
			return fmt.Errorf("stat source disk: %w", err)
		}
		if err := os.MkdirAll(contentDir, 0755); err != nil {
			return fmt.Errorf("create content directory: %w", err)
		}

		diskTemp, err := os.CreateTemp(contentDir, ".rootfs-*")
		if err != nil {
			return fmt.Errorf("create temporary content disk: %w", err)
		}
		diskTempPath := diskTemp.Name()
		if err := diskTemp.Close(); err != nil {
			_ = os.Remove(diskTempPath)
			return fmt.Errorf("close temporary content disk: %w", err)
		}
		if err := os.Remove(diskTempPath); err != nil {
			return fmt.Errorf("remove temporary content disk: %w", err)
		}
		if err := os.Link(sourceDiskPath, diskTempPath); err != nil {
			return fmt.Errorf("link source disk: %w", err)
		}
		installed := false
		defer func() {
			if !installed {
				_ = os.Remove(diskTempPath)
			}
		}()

		if err := os.Rename(diskTempPath, contentDigestPath(p, digestHex)); err != nil {
			return fmt.Errorf("install content disk: %w", err)
		}
		installed = true
		if err := writeMetadataFile(p.ImageContentMetadata(digestHex), sourceMeta); err != nil {
			return fmt.Errorf("write content metadata: %w", err)
		}
	}

	// A legacy source may still have tags pointing at its repository-local
	// digest directory. Move those references to the shared content before
	// removing the duplicate legacy tree.
	legacyDir := legacyDigestDir(p, sourceRepository, digestHex)
	if _, err := os.Stat(legacyDir); err == nil {
		tags, err := listTags(p, sourceRepository)
		if err != nil {
			return err
		}
		for _, tag := range tags {
			target, err := resolveTag(p, sourceRepository, tag)
			if err != nil || target != digestHex {
				continue
			}
			if err := createTagSymlink(p, sourceRepository, tag, digestHex); err != nil {
				return fmt.Errorf("promote legacy tag %s: %w", tag, err)
			}
		}
		if err := os.RemoveAll(legacyDir); err != nil {
			return fmt.Errorf("remove legacy digest directory: %w", err)
		}
	}

	return nil
}

// createTagSymlink creates or updates a tag symlink to point to a digest (only
// if the digest dir exists and the build is ready).
//
// Tag ownership is Docker last-pull-wins: the most recent pull of a tag always
// owns the symlink, regardless of platform. An earlier gate only repointed for
// host-native pulls, which silently stranded emulated variants (e.g.
// `pull --platform linux/amd64 alpine:3.19` could never make `image get` report
// amd64) and was non-recoverable. Always repointing is symmetric and matches
// Docker; callers repoint unconditionally on a ready digest.
func createTagSymlink(p *paths.Paths, repository, tag, digestHex string) error {
	layout := resolveImageLayout(p, repository, digestHex)
	linkPath := p.ImageTagSymlink(repository, tag)
	targetPath := digestHex // Relative path (just the digest hex)
	if layout.content {
		linkPath = newTagSymlinkPath(p, repository, tag)
		var err error
		targetPath, err = filepath.Rel(filepath.Dir(linkPath), layout.dir)
		if err != nil {
			return fmt.Errorf("calculate content symlink target: %w", err)
		}
	}
	if err := os.MkdirAll(filepath.Dir(linkPath), 0755); err != nil {
		return fmt.Errorf("create parent directory: %w", err)
	}

	linkTemp, err := os.CreateTemp(filepath.Dir(linkPath), ".tag-*")
	if err != nil {
		return fmt.Errorf("create temporary tag symlink: %w", err)
	}
	linkTempPath := linkTemp.Name()
	if err := linkTemp.Close(); err != nil {
		_ = os.Remove(linkTempPath)
		return fmt.Errorf("close temporary tag symlink: %w", err)
	}
	if err := os.Remove(linkTempPath); err != nil {
		return fmt.Errorf("remove temporary tag symlink: %w", err)
	}
	installed := false
	defer func() {
		if !installed {
			_ = os.Remove(linkTempPath)
		}
	}()

	if err := os.Symlink(targetPath, linkTempPath); err != nil {
		return fmt.Errorf("create symlink: %w", err)
	}
	if err := os.Rename(linkTempPath, linkPath); err != nil {
		return fmt.Errorf("install tag symlink: %w", err)
	}
	installed = true

	stalePath := p.ImageTagSymlink(repository, tag)
	if stalePath == linkPath {
		stalePath = newTagSymlinkPath(p, repository, tag)
	}
	if err := os.Remove(stalePath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove stale tag symlink: %w", err)
	}
	return nil
}

// resolveTag follows a tag symlink to get the digest hex
func resolveTag(p *paths.Paths, repository, tag string) (string, error) {
	linkPath := tagSymlinkPath(p, repository, tag)

	// Read the symlink
	target, err := os.Readlink(linkPath)
	if err != nil {
		if os.IsNotExist(err) {
			return "", ErrNotFound
		}
		return "", fmt.Errorf("read symlink: %w", err)
	}

	// Legacy links contain only the digest. New links point relatively into the
	// shared content directory; validate that they resolve to that digest only.
	if filepath.IsAbs(target) {
		return "", fmt.Errorf("invalid symlink target: %s", target)
	}
	digestHex := filepath.Base(target)
	if digestHex == "." || digestHex == string(filepath.Separator) {
		return "", fmt.Errorf("invalid symlink target: %s", target)
	}
	if target != digestHex {
		resolved := filepath.Clean(filepath.Join(filepath.Dir(linkPath), target))
		if resolved != filepath.Clean(contentDigestDir(p, digestHex)) {
			return "", fmt.Errorf("invalid symlink target: %s", target)
		}
	}

	return digestHex, nil
}

// listTags returns all tags for a repository
func listTags(p *paths.Paths, repository string) ([]string, error) {
	dirs := []string{p.ImageRepositoryDir(repository), filepath.Join(p.ImageRepositoriesDir(), repository)}
	seen := make(map[string]struct{})
	tags := make([]string, 0)
	for _, repoDir := range dirs {
		entries, err := os.ReadDir(repoDir)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, fmt.Errorf("read repository directory: %w", err)
		}
		for _, entry := range entries {
			path := filepath.Join(repoDir, entry.Name())
			info, err := os.Lstat(path)
			if err != nil || info.Mode()&os.ModeSymlink == 0 {
				continue
			}
			if _, ok := seen[entry.Name()]; ok {
				continue
			}
			seen[entry.Name()] = struct{}{}
			tags = append(tags, entry.Name())
		}
	}
	return tags, nil
}

// listAllMetadata returns one metadata record per digest across all repositories.
// Tagged images are discovered through tag symlinks, and digest-only images are
// discovered directly from their metadata.json files.
func listAllMetadata(p *paths.Paths) ([]*imageMetadata, error) {
	imagesDir := p.ImagesDir()
	seen := make(map[string]struct{})
	contentDigests := make(map[string]struct{})
	metas := make([]*imageMetadata, 0)

	err := filepath.Walk(imagesDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil // Skip errors
		}
		rel, err := filepath.Rel(imagesDir, path)
		if err != nil {
			return nil
		}
		parts := strings.Split(rel, string(filepath.Separator))
		if len(parts) > 0 && parts[0] == "content" {
			if info.IsDir() {
				return nil
			}
			if info.Name() != "metadata.json" {
				return nil
			}
			digestHex := filepath.Base(filepath.Dir(path))
			contentDigests[digestHex] = struct{}{}
			return nil
		}

		switch {
		case info.Mode()&os.ModeSymlink != 0:
			digestHex, err := os.Readlink(path)
			if err != nil {
				return nil // Skip invalid symlinks
			}
			digestHex = filepath.Base(digestHex)

			var repository, tag string
			if len(parts) > 1 && parts[0] == "repositories" {
				repository = filepath.Join(parts[1 : len(parts)-1]...)
				tag = parts[len(parts)-1]
			} else {
				repository = filepath.Dir(rel)
				tag = filepath.Base(path)
			}
			return appendMetadataForTag(p, repository, tag, digestHex, seen, &metas)
		case !info.IsDir() && info.Name() == "metadata.json":
			digestHex := filepath.Base(filepath.Dir(path))
			repository, err := filepath.Rel(imagesDir, filepath.Dir(filepath.Dir(path)))
			if err != nil {
				return nil
			}

			return appendMetadataIfNew(p, repository, digestHex, seen, &metas)
		default:
			return nil
		}
	})

	if err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("walk images directory: %w", err)
	}
	for digestHex := range contentDigests {
		found := false
		for _, meta := range metas {
			if strings.TrimPrefix(meta.Digest, "sha256:") == digestHex {
				found = true
				break
			}
		}
		if !found {
			if err := appendContentMetadataIfNew(p, digestHex, seen, &metas); err != nil {
				return nil, err
			}
		}
	}

	return metas, nil
}

func appendMetadataIfNew(p *paths.Paths, repository, digestHex string, seen map[string]struct{}, metas *[]*imageMetadata) error {
	key := repository + "@" + digestHex
	if _, ok := seen[key]; ok {
		return nil
	}

	meta, err := readMetadata(p, repository, digestHex)
	if err != nil {
		return nil // Skip if metadata can't be read
	}

	seen[key] = struct{}{}
	*metas = append(*metas, meta)
	return nil
}

func appendContentMetadataIfNew(p *paths.Paths, digestHex string, seen map[string]struct{}, metas *[]*imageMetadata) error {
	return appendMetadataIfNew(p, "", digestHex, seen, metas)
}

func appendMetadataForTag(p *paths.Paths, repository, tag, digestHex string, seen map[string]struct{}, metas *[]*imageMetadata) error {
	key := repository + "@" + digestHex
	if _, ok := seen[key]; ok {
		return nil
	}
	meta, err := readMetadata(p, repository, digestHex)
	if err != nil {
		return nil
	}
	meta.Name = repository + ":" + tag
	seen[key] = struct{}{}
	*metas = append(*metas, meta)
	return nil
}

// digestExists checks if a digest directory exists
func digestExists(p *paths.Paths, repository, digestHex string) bool {
	dir := digestDir(p, repository, digestHex)
	_, err := os.Stat(dir)
	return err == nil
}

// deleteTag removes a tag symlink (does not delete the digest directory)
func deleteTag(p *paths.Paths, repository, tag string) error {
	linkPath := tagSymlinkPath(p, repository, tag)

	// Check if symlink exists
	if _, err := os.Lstat(linkPath); err != nil {
		if os.IsNotExist(err) {
			return ErrNotFound
		}
		return fmt.Errorf("stat symlink: %w", err)
	}

	// Remove symlink
	if err := os.Remove(linkPath); err != nil {
		return fmt.Errorf("remove symlink: %w", err)
	}

	return nil
}

// countTagsForDigest counts how many tags in a repository point to a given digest
func countTagsForDigest(p *paths.Paths, repository, digestHex string) (int, error) {
	tags, err := listTags(p, repository)
	if err != nil {
		return 0, err
	}

	count := 0
	for _, tag := range tags {
		target, err := resolveTag(p, repository, tag)
		if err != nil {
			continue
		}
		if target == digestHex {
			count++
		}
	}
	return count, nil
}

func deleteTagsForDigest(p *paths.Paths, repository, digestHex string) error {
	tags, err := listTags(p, repository)
	if err != nil {
		return err
	}

	for _, tag := range tags {
		target, err := resolveTag(p, repository, tag)
		if err != nil {
			continue
		}
		if target != digestHex {
			continue
		}
		if err := deleteTag(p, repository, tag); err != nil && !errors.Is(err, ErrNotFound) {
			return err
		}
	}

	return nil
}

func contentTagsForDigest(p *paths.Paths, digestHex string) ([]string, error) {
	root := p.ImageRepositoriesDir()
	refs := make([]string, 0)
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || info.Mode()&os.ModeSymlink == 0 {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return nil
		}
		parts := strings.Split(rel, string(filepath.Separator))
		if len(parts) < 2 {
			return nil
		}
		repository := filepath.Join(parts[:len(parts)-1]...)
		tag := parts[len(parts)-1]
		target, err := resolveTag(p, repository, tag)
		if err == nil && target == digestHex {
			refs = append(refs, path)
		}
		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("walk content tags: %w", err)
	}
	return refs, nil
}

// deleteDigest removes a digest from both supported layouts.
func deleteDigest(p *paths.Paths, repository, digestHex string) error {
	for _, dir := range []string{
		contentDigestDir(p, digestHex),
		legacyDigestDir(p, repository, digestHex),
	} {
		if err := os.RemoveAll(dir); err != nil {
			return fmt.Errorf("remove digest directory: %w", err)
		}
	}
	return nil
}
