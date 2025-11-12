package images

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/onkernel/hypeman/lib/paths"
)

type imageMetadata struct {
	Name       string              `json:"name"`     // Normalized ref (tag or digest)
	Digest     string              `json:"digest"`   // Always present: sha256:...
	Status     string              `json:"status"`
	Error      *string             `json:"error,omitempty"`
	Request    *CreateImageRequest `json:"request,omitempty"`
	SizeBytes  int64               `json:"size_bytes"`
	Entrypoint []string            `json:"entrypoint,omitempty"`
	Cmd        []string            `json:"cmd,omitempty"`
	Env        map[string]string   `json:"env,omitempty"`
	WorkingDir string              `json:"working_dir,omitempty"`
	CreatedAt  time.Time           `json:"created_at"`
}

func (m *imageMetadata) toImage() *Image {
	img := &Image{
		Name:      m.Name,
		Digest:    m.Digest,
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
	if m.WorkingDir != "" {
		img.WorkingDir = m.WorkingDir
	}

	return img
}

// digestDir returns the directory for a specific digest
// e.g., /var/lib/hypeman/images/docker.io/library/alpine/abc123def456...
func digestDir(p *paths.Paths, repository, digestHex string) string {
	return p.ImageDigestDir(repository, digestHex)
}

// digestPath returns the path to the rootfs disk file for a digest
// Currently uses .ext4 extension (can change to .erofs when kernel supports it)
func digestPath(p *paths.Paths, repository, digestHex string) string {
	return p.ImageDigestPath(repository, digestHex)
}

// GetDiskPath returns the filesystem path to an image's rootfs.erofs file (public for instances manager)
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

// metadataPath returns the path to metadata.json for a digest
func metadataPath(p *paths.Paths, repository, digestHex string) string {
	return p.ImageMetadata(repository, digestHex)
}

// tagSymlinkPath returns the path to a tag symlink
// e.g., /var/lib/hypeman/images/docker.io/library/alpine/latest
func tagSymlinkPath(p *paths.Paths, repository, tag string) string {
	return p.ImageTagSymlink(repository, tag)
}

// writeMetadata writes metadata for a digest
func writeMetadata(p *paths.Paths, repository, digestHex string, meta *imageMetadata) error {
	dir := digestDir(p, repository, digestHex)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("create digest directory: %w", err)
	}

	data, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal metadata: %w", err)
	}

	tempPath := metadataPath(p, repository, digestHex) + ".tmp"
	if err := os.WriteFile(tempPath, data, 0644); err != nil {
		return fmt.Errorf("write temp metadata: %w", err)
	}

	finalPath := metadataPath(p, repository, digestHex)
	if err := os.Rename(tempPath, finalPath); err != nil {
		os.Remove(tempPath)
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

// createTagSymlink creates or updates a tag symlink to point to a digest
// Only creates the symlink if the digest dir exists and build is ready
func createTagSymlink(p *paths.Paths, repository, tag, digestHex string) error {
	linkPath := tagSymlinkPath(p, repository, tag)
	targetPath := digestHex // Relative path (just the digest hex)

	// Ensure parent directory exists
	if err := os.MkdirAll(filepath.Dir(linkPath), 0755); err != nil {
		return fmt.Errorf("create parent directory: %w", err)
	}

	// Remove existing symlink if present
	os.Remove(linkPath)

	// Create new symlink
	if err := os.Symlink(targetPath, linkPath); err != nil {
		return fmt.Errorf("create symlink: %w", err)
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

	// Validate it's just a digest hex (not an absolute path)
	if filepath.IsAbs(target) || strings.Contains(target, "/") {
		return "", fmt.Errorf("invalid symlink target: %s", target)
	}

	return target, nil
}

// listTags returns all tags for a repository
func listTags(p *paths.Paths, repository string) ([]string, error) {
	repoDir := p.ImageRepositoryDir(repository)
	
	entries, err := os.ReadDir(repoDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read repository directory: %w", err)
	}

	var tags []string
	for _, entry := range entries {
		// Check if it's a symlink
		info, err := os.Lstat(filepath.Join(repoDir, entry.Name()))
		if err != nil {
			continue
		}
		
		if info.Mode()&os.ModeSymlink != 0 {
			tags = append(tags, entry.Name())
		}
	}

	return tags, nil
}

// listAllTags returns all tags across all repositories
func listAllTags(p *paths.Paths) ([]*imageMetadata, error) {
	imagesDir := p.ImagesDir()
	var metas []*imageMetadata

	// Walk the images directory to find all repositories
	err := filepath.Walk(imagesDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil // Skip errors
		}

		// Check if this is a symlink (tag)
		if info.Mode()&os.ModeSymlink != 0 {
			// Read the symlink to get digest hex
			digestHex, err := os.Readlink(path)
			if err != nil {
				return nil // Skip invalid symlinks
			}

			// Get repository from path
			relPath, err := filepath.Rel(imagesDir, filepath.Dir(path))
			if err != nil {
				return nil
			}

			// Read metadata for this digest
			meta, err := readMetadata(p, relPath, digestHex)
			if err != nil {
				return nil // Skip if metadata can't be read
			}

			metas = append(metas, meta)
		}

		return nil
	})

	if err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("walk images directory: %w", err)
	}

	return metas, nil
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
