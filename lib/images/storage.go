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
	Name              string              `json:"name"`   // Normalized ref (tag or digest)
	Digest            string              `json:"digest"` // Always present: sha256:...
	Platform          string              `json:"platform,omitempty"`
	Status            string              `json:"status"`
	Error             *string             `json:"error,omitempty"`
	Request           *CreateImageRequest `json:"request,omitempty"`
	SizeBytes         int64               `json:"size_bytes"`
	Entrypoint        []string            `json:"entrypoint,omitempty"`
	Cmd               []string            `json:"cmd,omitempty"`
	Env               map[string]string   `json:"env,omitempty"`
	Labels            map[string]string   `json:"labels,omitempty"`
	Tags              tags.Tags           `json:"tags,omitempty"`
	WorkingDir        string              `json:"working_dir,omitempty"`
	CreatedAt         time.Time           `json:"created_at"`
	BorrowedAuth      bool                `json:"borrowed_auth,omitempty"`
	BuildID           string              `json:"build_id,omitempty"`
	RequestedTag      string              `json:"requested_tag,omitempty"`
	PreviousTagDigest string              `json:"previous_tag_digest,omitempty"`
	TagGeneration     uint64              `json:"tag_generation,omitempty"`
}

func (m *imageMetadata) toImageFor(reference string) *Image {
	img := m.toImage()
	img.Name = reference
	return img
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

// imageLayout contains the paths for one image layout.
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
		dir:      p.ImageDigestDir(repository, digestHex),
		metadata: p.ImageMetadata(repository, digestHex),
		disk:     p.ImageDigestPath(repository, digestHex),
	}
	content := contentLayout(p, digestHex)

	if legacyImageExists(p, repository, digestHex) {
		contentStatus, contentOK := metadataStatus(content.metadata)
		if !contentOK || contentStatus != StatusReady {
			return legacy
		}
	}
	if pathExists(content.metadata) || pathExists(content.disk) {
		return content
	}
	if pathExists(legacy.metadata) {
		return legacy
	}
	return content
}

func contentLayout(p *paths.Paths, digestHex string) imageLayout {
	return imageLayout{
		dir:      p.ImageContentDir(digestHex),
		metadata: p.ImageContentMetadata(digestHex),
		disk:     p.ImageContentPath(digestHex),
		content:  true,
	}
}

func pathExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func digestDir(p *paths.Paths, repository, digestHex string) string {
	return resolveImageLayout(p, repository, digestHex).dir
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

// GetDiskPath returns the filesystem path to an image's rootfs disk file (public for instances manager)
func GetDiskPath(p *paths.Paths, imageName string, digest string) (string, error) {
	// Parse image name to get repository
	ref, err := ParseNormalizedRef(imageName)
	if err != nil {
		return "", fmt.Errorf("parse image name: %w", err)
	}

	// Extract digest hex (remove "sha256:" prefix)
	digestHex := strings.TrimPrefix(digest, "sha256:")

	return resolveImageLayout(p, ref.Repository(), digestHex).disk, nil
}

func digestPath(p *paths.Paths, repository, digestHex string) string {
	return resolveImageLayout(p, repository, digestHex).disk
}

func metadataPath(p *paths.Paths, repository, digestHex string) string {
	return resolveImageLayout(p, repository, digestHex).metadata
}

func tagSymlinkPath(p *paths.Paths, repository, tag string) string {
	newPath := p.ImageRepositoryTagSymlink(repository, tag)
	if _, err := os.Lstat(newPath); err == nil {
		return newPath
	}
	return p.ImageTagSymlink(repository, tag)
}

func writeMetadata(p *paths.Paths, repository, digestHex string, meta *imageMetadata) error {
	return writeMetadataFile(resolveImageLayout(p, repository, digestHex).metadata, meta)
}

func writeMetadataFile(path string, meta *imageMetadata) error {
	data, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal metadata: %w", err)
	}
	return writeJSONAtomic(path, data)
}

func readMetadata(p *paths.Paths, repository, digestHex string) (*imageMetadata, error) {
	return readMetadataAt(resolveImageLayout(p, repository, digestHex))
}

// resolveRefMetadata resolves a digest reference directly or a tag reference
// through its symlink, then reads the metadata it points at.
func resolveRefMetadata(p *paths.Paths, ref *NormalizedRef) (string, *imageMetadata, error) {
	digestHex := ref.DigestHex()
	if !ref.IsDigest() {
		var err error
		digestHex, err = resolveTag(p, ref.Repository(), ref.Tag())
		if err != nil {
			return "", nil, err
		}
	}
	meta, err := readMetadata(p, ref.Repository(), digestHex)
	if err != nil {
		return "", nil, err
	}
	return digestHex, meta, nil
}

func readContentMetadata(p *paths.Paths, digestHex string) (*imageMetadata, error) {
	return readMetadataAt(contentLayout(p, digestHex))
}

func readMetadataAt(layout imageLayout) (*imageMetadata, error) {
	path := layout.metadata
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
		if _, err := os.Stat(layout.disk); err != nil {
			if os.IsNotExist(err) {
				return nil, fmt.Errorf("disk image missing: %s", layout.disk)
			}
			return nil, fmt.Errorf("stat disk image: %w", err)
		}
	}

	return &meta, nil
}

func promoteImageToContent(p *paths.Paths, sourceRepository, digestHex string, sourceMeta *imageMetadata) error {
	contentMeta, err := readContentMetadata(p, digestHex)
	if err == nil {
		if contentMeta.Status == StatusReady {
			return promoteLegacyTags(p, sourceRepository, digestHex)
		}
		return fmt.Errorf("%w: content status is %s", ErrImageNotReady, contentMeta.Status)
	}
	if !errors.Is(err, ErrNotFound) {
		return err
	}

	sourceLayout := resolveImageLayout(p, sourceRepository, digestHex)
	sourceDiskPath := sourceLayout.disk
	if _, err := os.Stat(sourceDiskPath); err != nil {
		return fmt.Errorf("stat source disk: %w", err)
	}
	if err := os.MkdirAll(p.ImageContentDir(digestHex), 0755); err != nil {
		return fmt.Errorf("create content directory: %w", err)
	}
	pendingMeta := *sourceMeta
	pendingMeta.Status = StatusConverting
	if err := writeMetadataFile(p.ImageContentMetadata(digestHex), &pendingMeta); err != nil {
		return fmt.Errorf("write content metadata: %w", err)
	}
	if err := installAtomically(p.ImageContentPath(digestHex), func(path string) error {
		return os.Link(sourceDiskPath, path)
	}); err != nil {
		return fmt.Errorf("link source disk: %w", err)
	}
	if err := writeMetadataFile(p.ImageContentMetadata(digestHex), sourceMeta); err != nil {
		return fmt.Errorf("finalize content metadata: %w", err)
	}

	return promoteLegacyTags(p, sourceRepository, digestHex)
}

func promoteLegacyTags(p *paths.Paths, repository, digestHex string) error {
	if _, err := os.Stat(p.ImageDigestDir(repository, digestHex)); err != nil {
		return nil
	}
	tags, err := listTags(p, repository)
	if err != nil {
		return err
	}
	staged := make([]stagedTagSymlink, 0, len(tags))
	defer func() {
		for _, ref := range staged {
			_ = os.RemoveAll(ref.tempDir)
		}
	}()
	for _, tag := range tags {
		target, err := resolveTag(p, repository, tag)
		if err != nil || target != digestHex {
			continue
		}
		// resolveTag validated that content-relative links resolve to this
		// digest's content dir, so only legacy links (bare digest target)
		// still need restaging.
		if raw, err := os.Readlink(tagSymlinkPath(p, repository, tag)); err == nil && raw != digestHex {
			continue
		}
		ref, err := stageTagSymlink(p, repository, tag, digestHex)
		if err != nil {
			return fmt.Errorf("stage legacy tag %s: %w", tag, err)
		}
		staged = append(staged, ref)
	}
	for i, ref := range staged {
		if err := os.Rename(ref.tempPath, ref.linkPath); err != nil {
			if rollbackErr := rollbackTagSymlinks(staged[:i]); rollbackErr != nil {
				return errors.Join(fmt.Errorf("promote legacy tag: %w", err), rollbackErr)
			}
			return fmt.Errorf("promote legacy tag: %w", err)
		}
	}
	return nil
}

func installAtomically(path string, install func(string) error) error {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	tempDir, err := os.MkdirTemp(filepath.Dir(path), ".install-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tempDir)
	tempPath := filepath.Join(tempDir, filepath.Base(path))
	if err := install(tempPath); err != nil {
		return err
	}
	return os.Rename(tempPath, path)
}

type symlinkState struct {
	exists bool
	target string
}

type stagedTagSymlink struct {
	repository    string
	tag           string
	linkPath      string
	tempDir       string
	tempPath      string
	previous      symlinkState
	stalePath     string
	previousStale symlinkState
}

func stageTagSymlink(p *paths.Paths, repository, tag, digestHex string) (stagedTagSymlink, error) {
	layout := resolveImageLayout(p, repository, digestHex)
	linkPath := p.ImageTagSymlink(repository, tag)
	targetPath := digestHex // Relative path (just the digest hex)
	if layout.content {
		linkPath = p.ImageRepositoryTagSymlink(repository, tag)
		var err error
		targetPath, err = filepath.Rel(filepath.Dir(linkPath), layout.dir)
		if err != nil {
			return stagedTagSymlink{}, fmt.Errorf("calculate content symlink target: %w", err)
		}
	}
	previous, err := readSymlinkState(linkPath)
	if err != nil {
		return stagedTagSymlink{}, err
	}
	stalePath := p.ImageTagSymlink(repository, tag)
	if stalePath == linkPath {
		stalePath = p.ImageRepositoryTagSymlink(repository, tag)
	}
	previousStale, err := readSymlinkState(stalePath)
	if err != nil {
		return stagedTagSymlink{}, err
	}
	if err := os.MkdirAll(filepath.Dir(linkPath), 0755); err != nil {
		return stagedTagSymlink{}, fmt.Errorf("create parent directory: %w", err)
	}
	tempDir, err := os.MkdirTemp(filepath.Dir(linkPath), ".tag-stage-*")
	if err != nil {
		return stagedTagSymlink{}, fmt.Errorf("create temporary tag directory: %w", err)
	}
	tempPath := filepath.Join(tempDir, filepath.Base(linkPath))
	if err := os.Symlink(targetPath, tempPath); err != nil {
		_ = os.RemoveAll(tempDir)
		return stagedTagSymlink{}, fmt.Errorf("create temporary tag symlink: %w", err)
	}
	return stagedTagSymlink{
		repository:    repository,
		tag:           tag,
		linkPath:      linkPath,
		tempDir:       tempDir,
		tempPath:      tempPath,
		previous:      previous,
		stalePath:     stalePath,
		previousStale: previousStale,
	}, nil
}

func readSymlinkState(path string) (symlinkState, error) {
	target, err := os.Readlink(path)
	if err != nil {
		if os.IsNotExist(err) {
			return symlinkState{}, nil
		}
		return symlinkState{}, fmt.Errorf("read existing tag symlink: %w", err)
	}
	return symlinkState{exists: true, target: target}, nil
}

func restoreSymlinkState(path string, state symlinkState) error {
	if !state.exists {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove tag symlink during rollback: %w", err)
		}
		return nil
	}
	if err := installAtomically(path, func(tempPath string) error {
		return os.Symlink(state.target, tempPath)
	}); err != nil {
		return fmt.Errorf("restore tag symlink during rollback: %w", err)
	}
	return nil
}

func rollbackTagSymlinks(refs []stagedTagSymlink) error {
	var rollbackErrs []error
	for i := len(refs) - 1; i >= 0; i-- {
		if err := restoreTagSymlink(&refs[i]); err != nil {
			rollbackErrs = append(rollbackErrs, err)
		}
	}
	return errors.Join(rollbackErrs...)
}

func removeStaleTagSymlink(p *paths.Paths, ref *stagedTagSymlink) error {
	stalePath := p.ImageTagSymlink(ref.repository, ref.tag)
	if stalePath == ref.linkPath {
		stalePath = p.ImageRepositoryTagSymlink(ref.repository, ref.tag)
	}
	if err := os.Remove(stalePath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove stale tag symlink: %w", err)
	}
	return nil
}

func installTagSymlink(p *paths.Paths, repository, tag, digestHex string) (stagedTagSymlink, error) {
	ref, err := stageTagSymlink(p, repository, tag, digestHex)
	if err != nil {
		return stagedTagSymlink{}, fmt.Errorf("stage tag symlink: %w", err)
	}
	if err := os.Rename(ref.tempPath, ref.linkPath); err != nil {
		_ = os.RemoveAll(ref.tempDir)
		return stagedTagSymlink{}, fmt.Errorf("install tag symlink: %w", err)
	}
	if err := removeStaleTagSymlink(p, &ref); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to remove stale tag symlink %s: %v\n", tag, err)
	}
	_ = os.RemoveAll(ref.tempDir)
	return ref, nil
}

func createTagSymlink(p *paths.Paths, repository, tag, digestHex string) error {
	_, err := installTagSymlink(p, repository, tag, digestHex)
	return err
}

func restoreTagSymlink(ref *stagedTagSymlink) error {
	if err := restoreSymlinkState(ref.linkPath, ref.previous); err != nil {
		return err
	}
	return restoreSymlinkState(ref.stalePath, ref.previousStale)
}

// errInvalidSymlinkTarget marks a tag symlink that does not resolve to a
// digest or the shared content directory.
var errInvalidSymlinkTarget = errors.New("invalid symlink target")

func resolveTag(p *paths.Paths, repository, tag string) (string, error) {
	linkPath := tagSymlinkPath(p, repository, tag)

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
		return "", fmt.Errorf("%w: %s", errInvalidSymlinkTarget, target)
	}
	digestHex := filepath.Base(target)
	if digestHex == "." || digestHex == string(filepath.Separator) {
		return "", fmt.Errorf("%w: %s", errInvalidSymlinkTarget, target)
	}
	if target != digestHex {
		resolved := filepath.Clean(filepath.Join(filepath.Dir(linkPath), target))
		if resolved != filepath.Clean(p.ImageContentDir(digestHex)) {
			return "", fmt.Errorf("%w: %s", errInvalidSymlinkTarget, target)
		}
	}

	return digestHex, nil
}
