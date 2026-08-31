package images

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/kernel/hypeman/lib/paths"
)

func ensurePendingTag(p *paths.Paths, repository, tag, digestHex string) error {
	_, err := resolveTag(p, repository, tag)
	if err == nil {
		return nil
	}
	if !errors.Is(err, ErrNotFound) {
		return err
	}
	return createTagSymlink(p, repository, tag, digestHex)
}

func listTags(p *paths.Paths, repository string) ([]string, error) {
	dirs := []string{filepath.Join(p.ImageRepositoriesDir(), repository), p.ImageRepositoryDir(repository)}
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
			if entry.Type()&os.ModeSymlink == 0 {
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

type legacyRef struct {
	repository string
	digestHex  string
}

func collectLegacyImages(p *paths.Paths) ([]legacyRef, error) {
	imagesDir := p.ImagesDir()
	refs := make([]legacyRef, 0)
	err := filepath.Walk(imagesDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || info.Name() != "metadata.json" {
			return nil
		}
		rel, relErr := filepath.Rel(imagesDir, path)
		if relErr != nil {
			return nil
		}
		parts := strings.Split(rel, string(filepath.Separator))
		if len(parts) < 3 || parts[0] == "content" || parts[0] == "repositories" {
			return nil
		}
		refs = append(refs, legacyRef{
			repository: filepath.Join(parts[:len(parts)-2]...),
			digestHex:  parts[len(parts)-2],
		})
		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	return refs, nil
}

func promoteLegacyImages(p *paths.Paths, refs []legacyRef) {
	for _, ref := range refs {
		layout := resolveImageLayout(p, ref.repository, ref.digestHex)
		meta, readErr := readMetadataAt(layout)
		if readErr != nil || meta.Status != StatusReady {
			continue
		}
		if promoteErr := promoteImageToContent(p, ref.repository, ref.digestHex, meta); promoteErr != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to promote legacy image %s@%s: %v\n", ref.repository, ref.digestHex, promoteErr)
		}
	}
}

func listAllMetadata(p *paths.Paths) ([]*imageMetadata, error) {
	imagesDir := p.ImagesDir()
	seen := make(map[string]struct{})
	contentDigests := make(map[string]struct{})
	taggedDigests := make(map[string]struct{})
	taggedContentDigests := make(map[string]struct{})
	metadataRefs := make([]metadataReference, 0)
	seenMetadataRefs := make(map[string]struct{})
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
			return appendMetadataForTag(p, repository, tag, digestHex, seen, taggedDigests, taggedContentDigests, &metas)
		case !info.IsDir() && info.Name() == "metadata.json":
			digestHex := filepath.Base(filepath.Dir(path))
			repository, err := filepath.Rel(imagesDir, filepath.Dir(filepath.Dir(path)))
			if err != nil {
				return nil
			}
			key := repository + "@" + digestHex
			if _, ok := seenMetadataRefs[key]; !ok {
				seenMetadataRefs[key] = struct{}{}
				metadataRefs = append(metadataRefs, metadataReference{repository: repository, digestHex: digestHex})
			}
			return nil
		default:
			return nil
		}
	})

	if err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("walk images directory: %w", err)
	}
	seenDigests := make(map[string]struct{}, len(metas))
	for _, ref := range metadataRefs {
		if _, tagged := taggedDigests[ref.repository+"@"+ref.digestHex]; tagged {
			continue
		}
		appendMetadataIfNew(p, ref.repository, ref.digestHex, seen, &metas)
		seenDigests[ref.digestHex] = struct{}{}
	}
	for digestHex := range contentDigests {
		if _, found := taggedContentDigests[digestHex]; found {
			continue
		}
		if _, found := seenDigests[digestHex]; found {
			continue
		}
		appendContentMetadataIfNew(p, digestHex, seen, &metas)
	}

	return metas, nil
}

type metadataReference struct {
	repository string
	digestHex  string
}

func appendMetadataIfNew(p *paths.Paths, repository, digestHex string, seen map[string]struct{}, metas *[]*imageMetadata) {
	key := repository + "@" + digestHex
	if _, ok := seen[key]; ok {
		return
	}

	meta, err := readMetadata(p, repository, digestHex)
	if err != nil {
		return
	}

	seen[key] = struct{}{}
	*metas = append(*metas, meta)
}

func appendContentMetadataIfNew(p *paths.Paths, digestHex string, seen map[string]struct{}, metas *[]*imageMetadata) {
	key := "@" + digestHex
	if _, ok := seen[key]; ok {
		return
	}
	meta, err := readContentMetadata(p, digestHex)
	if err != nil {
		return
	}
	seen[key] = struct{}{}
	*metas = append(*metas, meta)
}

func appendMetadataForTag(p *paths.Paths, repository, tag, digestHex string, seen, taggedDigests, taggedContentDigests map[string]struct{}, metas *[]*imageMetadata) error {
	tagKey := repository + ":" + tag
	if _, ok := seen[tagKey]; ok {
		return nil
	}
	meta, err := readMetadata(p, repository, digestHex)
	if err != nil {
		return nil
	}
	meta.Name = repository + ":" + tag
	seen[tagKey] = struct{}{}
	taggedDigests[repository+"@"+digestHex] = struct{}{}
	taggedContentDigests[digestHex] = struct{}{}
	*metas = append(*metas, meta)
	return nil
}

func deleteTag(p *paths.Paths, repository, tag string) error {
	pathsToRemove := []string{
		p.ImageRepositoryTagSymlink(repository, tag),
		p.ImageTagSymlink(repository, tag),
	}
	found := false
	for _, linkPath := range pathsToRemove {
		if _, err := os.Lstat(linkPath); err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return fmt.Errorf("stat symlink: %w", err)
		}
		found = true
		if err := os.Remove(linkPath); err != nil {
			return fmt.Errorf("remove symlink: %w", err)
		}
	}
	if !found {
		return ErrNotFound
	}
	return nil
}

func tagsForDigest(p *paths.Paths, repository, digestHex string) ([]string, error) {
	tags, err := listTags(p, repository)
	if err != nil {
		return nil, err
	}
	matched := make([]string, 0)
	for _, tag := range tags {
		target, err := resolveTag(p, repository, tag)
		if err == nil && target == digestHex {
			matched = append(matched, tag)
		}
	}
	return matched, nil
}

func countTagsForDigest(p *paths.Paths, repository, digestHex string) (int, error) {
	tags, err := tagsForDigest(p, repository, digestHex)
	return len(tags), err
}

func deleteTagsForDigest(p *paths.Paths, repository, digestHex string) error {
	tags, err := tagsForDigest(p, repository, digestHex)
	if err != nil {
		return err
	}
	for _, tag := range tags {
		if err := deleteTag(p, repository, tag); err != nil && !errors.Is(err, ErrNotFound) {
			return err
		}
	}
	return nil
}

func contentTagCount(p *paths.Paths, digestHex string) (int, error) {
	root := p.ImageRepositoriesDir()
	count := 0
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
		if target, err := resolveTag(p, repository, parts[len(parts)-1]); err == nil && target == digestHex {
			count++
		}
		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		return 0, fmt.Errorf("walk content tags: %w", err)
	}
	return count, nil
}

func contentPullInProgress(p *paths.Paths, digestHex string) bool {
	status, ok := metadataStatus(p.ImageContentMetadata(digestHex))
	return ok && (status == StatusPending || status == StatusPulling || status == StatusConverting)
}

func contentIsDigestOnly(p *paths.Paths, digestHex string) bool {
	meta, err := readContentMetadata(p, digestHex)
	if err != nil {
		return false
	}
	ref, err := ParseNormalizedRef(meta.Name)
	return err == nil && ref.IsDigest()
}

func removeDigestIfUnreferenced(p *paths.Paths, repository, digestHex string, preserveDigestOnly bool) error {
	contentDir := p.ImageContentDir(digestHex)
	contentExists := false
	if _, err := os.Stat(contentDir); err == nil {
		contentExists = true
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("stat content digest directory: %w", err)
	}

	if err := os.RemoveAll(p.ImageDigestDir(repository, digestHex)); err != nil {
		return fmt.Errorf("remove legacy digest directory: %w", err)
	}
	if !contentExists {
		return nil
	}

	tagCount, err := contentTagCount(p, digestHex)
	if err != nil {
		return err
	}
	if tagCount > 0 || contentPullInProgress(p, digestHex) || (preserveDigestOnly && contentIsDigestOnly(p, digestHex)) {
		return nil
	}

	if err := os.RemoveAll(contentDir); err != nil {
		return fmt.Errorf("remove content digest directory: %w", err)
	}
	return nil
}
