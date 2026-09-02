package images

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/kernel/hypeman/lib/paths"
)

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

type metadataIndex struct {
	seen                 map[string]struct{}
	contentDigests       map[string]struct{}
	taggedDigests        map[string]struct{}
	taggedContentDigests map[string]struct{}
	metadataRefs         []metadataReference
	seenMetadataRefs     map[string]struct{}
	metas                []*imageMetadata
}

func listAllMetadata(p *paths.Paths) ([]*imageMetadata, error) {
	index := metadataIndex{
		seen:                 make(map[string]struct{}),
		contentDigests:       make(map[string]struct{}),
		taggedDigests:        make(map[string]struct{}),
		taggedContentDigests: make(map[string]struct{}),
		metadataRefs:         make([]metadataReference, 0),
		seenMetadataRefs:     make(map[string]struct{}),
		metas:                make([]*imageMetadata, 0),
	}
	if err := index.walk(p); err != nil {
		return nil, err
	}
	index.appendUnreferenced(p)
	return index.metas, nil
}

func (i *metadataIndex) walk(p *paths.Paths) error {
	imagesDir := p.ImagesDir()
	err := filepath.Walk(imagesDir, func(path string, info os.FileInfo, err error) error {
		return i.visit(p, imagesDir, path, info, err)
	})
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("walk images directory: %w", err)
	}
	return nil
}

func (i *metadataIndex) visit(p *paths.Paths, imagesDir, path string, info os.FileInfo, walkErr error) error {
	if walkErr != nil {
		return nil
	}
	rel, err := filepath.Rel(imagesDir, path)
	if err != nil {
		return nil
	}
	parts := strings.Split(rel, string(filepath.Separator))
	if len(parts) > 0 && parts[0] == "content" {
		return i.visitContent(path, info)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return i.visitTag(p, path, rel, parts)
	}
	if !info.IsDir() && info.Name() == "metadata.json" {
		i.recordMetadataRef(imagesDir, path)
	}
	return nil
}

func (i *metadataIndex) visitContent(path string, info os.FileInfo) error {
	if info.IsDir() || info.Name() != "metadata.json" {
		return nil
	}
	i.contentDigests[filepath.Base(filepath.Dir(path))] = struct{}{}
	return nil
}

func (i *metadataIndex) visitTag(p *paths.Paths, path, rel string, parts []string) error {
	digestHex, err := os.Readlink(path)
	if err != nil {
		return nil
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
	return i.appendMetadataForTag(p, repository, tag, digestHex)
}

func (i *metadataIndex) recordMetadataRef(imagesDir, path string) {
	digestHex := filepath.Base(filepath.Dir(path))
	repository, err := filepath.Rel(imagesDir, filepath.Dir(filepath.Dir(path)))
	if err != nil {
		return
	}
	key := repository + "@" + digestHex
	if _, ok := i.seenMetadataRefs[key]; ok {
		return
	}
	i.seenMetadataRefs[key] = struct{}{}
	i.metadataRefs = append(i.metadataRefs, metadataReference{repository: repository, digestHex: digestHex})
}

func (i *metadataIndex) appendUnreferenced(p *paths.Paths) {
	seenDigests := make(map[string]struct{}, len(i.metas))
	for _, ref := range i.metadataRefs {
		if _, tagged := i.taggedDigests[ref.repository+"@"+ref.digestHex]; tagged {
			continue
		}
		appendMetadataIfNew(p, ref.repository, ref.digestHex, i.seen, &i.metas)
		seenDigests[ref.digestHex] = struct{}{}
	}
	for digestHex := range i.contentDigests {
		if _, found := i.taggedContentDigests[digestHex]; found {
			continue
		}
		if _, found := seenDigests[digestHex]; found {
			continue
		}
		appendContentMetadataIfNew(p, digestHex, i.seen, &i.metas)
	}
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

func (i *metadataIndex) appendMetadataForTag(p *paths.Paths, repository, tag, digestHex string) error {
	tagKey := repository + ":" + tag
	if _, ok := i.seen[tagKey]; ok {
		return nil
	}
	meta, err := readMetadata(p, repository, digestHex)
	if err != nil {
		return nil
	}
	clone := *meta
	clone.Name = repository + ":" + tag
	i.seen[tagKey] = struct{}{}
	i.taggedDigests[repository+"@"+digestHex] = struct{}{}
	i.taggedContentDigests[digestHex] = struct{}{}
	i.metas = append(i.metas, &clone)
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

func deleteTags(p *paths.Paths, repository string, tags []string) error {
	for _, tag := range tags {
		if err := deleteTag(p, repository, tag); err != nil && !errors.Is(err, ErrNotFound) {
			return err
		}
	}
	return nil
}

func deleteTagsForDigest(p *paths.Paths, repository string, tags []string) error {
	return deleteTags(p, repository, tags)
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
	return ok && isPendingImageStatus(status)
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
