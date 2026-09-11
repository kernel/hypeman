package images

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

func walkWithContext(ctx context.Context, root string, walkFn filepath.WalkFunc) error {
	return filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		return walkFn(path, info, err)
	})
}

// totalReadyImageBytesFromMetadata sums ready image sizes directly from metadata.json files.
// This is conservative for admission control and disk accounting: if metadata says an
// image is ready, we count its recorded size without re-validating the rootfs path. If
// the metadata file is unreadable or malformed, we fall back to counting any rootfs disk
// files found in the digest directory so we do not undercount host disk usage.
func totalReadyImageBytesFromMetadataWithContext(ctx context.Context, imagesDir string) (int64, error) {
	var total int64
	seenRootfs := make(map[rootfsIdentity]struct{})
	layersDir := filepath.Join(imagesDir, "layers")

	err := walkWithContext(ctx, imagesDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if info.IsDir() {
			if filepath.Clean(path) == filepath.Clean(layersDir) {
				return filepath.SkipDir
			}
			return nil
		}
		if info.Name() != "metadata.json" {
			return nil
		}

		data, err := os.ReadFile(path)
		if err != nil {
			rootfsBytes, fallbackErr := totalRootfsBytesInDigestDirWithContext(ctx, filepath.Dir(path), seenRootfs)
			if fallbackErr == nil {
				total += rootfsBytes
				return nil
			}
			if ctxErr := ctx.Err(); ctxErr != nil {
				return ctxErr
			}
			return fmt.Errorf("read image metadata %s: %w", path, err)
		}

		var meta imageMetadata
		if err := json.Unmarshal(data, &meta); err != nil {
			rootfsBytes, fallbackErr := totalRootfsBytesInDigestDirWithContext(ctx, filepath.Dir(path), seenRootfs)
			if fallbackErr == nil {
				total += rootfsBytes
				return nil
			}
			if ctxErr := ctx.Err(); ctxErr != nil {
				return ctxErr
			}
			return fmt.Errorf("unmarshal image metadata %s: %w", path, err)
		}
		if meta.Status == StatusReady {
			rootfsPaths, globErr := filepath.Glob(filepath.Join(filepath.Dir(path), "rootfs.*"))
			if globErr != nil {
				return fmt.Errorf("find ready image rootfs for %s: %w", path, globErr)
			}
			for _, rootfsPath := range rootfsPaths {
				rootfsInfo, statErr := os.Stat(rootfsPath)
				if statErr != nil {
					continue
				}
				if !markUniqueRootfs(rootfsInfo, seenRootfs) {
					return nil
				}
				break
			}

			if meta.SizeBytes > 0 {
				total += meta.SizeBytes
				return nil
			}
			rootfsBytes, err := totalRootfsBytesInDigestDirWithContext(ctx, filepath.Dir(path), nil)
			if err != nil {
				return fmt.Errorf("stat ready image rootfs for %s: %w", path, err)
			}
			total += rootfsBytes
		}
		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		return 0, fmt.Errorf("walk images directory: %w", err)
	}

	return total, nil
}

func totalFileBytesWithContext(ctx context.Context, dir, label string) (int64, error) {
	var total int64
	err := walkWithContext(ctx, dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if !info.IsDir() {
			total += info.Size()
		}
		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		return 0, fmt.Errorf("walk %s: %w", label, err)
	}
	return total, nil
}

// totalLayerArtifactBytesFromFilesystem includes records and in-progress trees.
func totalLayerArtifactBytesFromFilesystemWithContext(ctx context.Context, layersDir string) (int64, error) {
	return totalFileBytesWithContext(ctx, layersDir, "layer artifacts")
}

// totalOCICacheBlobBytesFromFilesystem includes unreferenced blobs still on disk.
func totalOCICacheBlobBytesFromFilesystemWithContext(ctx context.Context, blobDir string) (int64, error) {
	return totalFileBytesWithContext(ctx, blobDir, "OCI cache blobs")
}

func (s *layerStore) getDiskUsageTotals(ctx context.Context) (int64, int64, error) {
	for attempt := 0; ; attempt++ {
		if err := ctx.Err(); err != nil {
			return 0, 0, err
		}
		s.diskUsageMu.RLock()
		generation := s.diskUsageGeneration
		if s.diskUsageLoaded {
			readyImageBytes := s.readyImageBytes
			ociCacheBytes := s.ociCacheBytes
			s.diskUsageMu.RUnlock()
			return readyImageBytes, ociCacheBytes, nil
		}
		s.diskUsageMu.RUnlock()

		readyImageBytes, ociCacheBytes, err := s.computeDiskUsageTotals(ctx)
		if err != nil {
			return 0, 0, err
		}

		s.diskUsageMu.Lock()
		if s.diskUsageGeneration != generation {
			s.diskUsageMu.Unlock()
			if attempt >= 2 {
				return 0, 0, fmt.Errorf("disk usage changed during scan")
			}
			continue
		}
		if s.activeBuilds == 0 {
			if !s.diskUsageLoaded {
				s.readyImageBytes = readyImageBytes
				s.ociCacheBytes = ociCacheBytes
				s.diskUsageLoaded = true
			}
			readyImageBytes = s.readyImageBytes
			ociCacheBytes = s.ociCacheBytes
		}
		s.diskUsageMu.Unlock()

		return readyImageBytes, ociCacheBytes, nil
	}
}

func (s *layerStore) invalidateDiskUsageTotals() {
	s.diskUsageMu.Lock()
	s.diskUsageGeneration++
	s.diskUsageLoaded = false
	s.diskUsageMu.Unlock()
}

func (s *layerStore) beginLayerBuild() {
	s.diskUsageMu.Lock()
	s.activeBuilds++
	s.diskUsageGeneration++
	s.diskUsageLoaded = false
	s.diskUsageMu.Unlock()
}

func (s *layerStore) endLayerBuild() {
	s.diskUsageMu.Lock()
	s.activeBuilds--
	s.diskUsageGeneration++
	s.diskUsageLoaded = false
	s.diskUsageMu.Unlock()
}

func (s *layerStore) refreshDiskUsageTotals() {
	s.diskUsageMu.RLock()
	generation := s.diskUsageGeneration
	s.diskUsageMu.RUnlock()

	readyImageBytes, ociCacheBytes, err := s.computeDiskUsageTotals(context.Background())
	if err != nil {
		return
	}

	s.diskUsageMu.Lock()
	if s.diskUsageGeneration == generation && s.activeBuilds == 0 {
		s.readyImageBytes = readyImageBytes
		s.ociCacheBytes = ociCacheBytes
		s.diskUsageLoaded = true
	}
	s.diskUsageMu.Unlock()
}

func (s *layerStore) computeDiskUsageTotals(ctx context.Context) (int64, int64, error) {
	readyImageBytes, err := totalReadyImageBytesFromMetadataWithContext(ctx, s.paths.ImagesDir())
	if err != nil {
		return 0, 0, err
	}
	ociCacheBytes, err := totalOCICacheBlobBytesFromFilesystemWithContext(ctx, s.paths.OCICacheBlobDir())
	if err != nil {
		return 0, 0, err
	}
	layerArtifactBytes, err := totalLayerArtifactBytesFromFilesystemWithContext(ctx, s.paths.ImageLayersDir())
	if err != nil {
		return 0, 0, err
	}
	return readyImageBytes, ociCacheBytes + layerArtifactBytes, nil
}

func totalRootfsBytesInDigestDirWithContext(ctx context.Context, digestDir string, seen map[rootfsIdentity]struct{}) (int64, error) {
	rootfsPaths, err := filepath.Glob(filepath.Join(digestDir, "rootfs.*"))
	if err != nil {
		return 0, err
	}
	if len(rootfsPaths) == 0 {
		return 0, os.ErrNotExist
	}

	var total int64
	found := false
	for _, rootfsPath := range rootfsPaths {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		info, err := os.Stat(rootfsPath)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return 0, err
		}
		if info.IsDir() {
			continue
		}
		found = true
		if seen != nil && !markUniqueRootfs(info, seen) {
			continue
		}
		total += info.Size()
	}
	if !found || seen == nil && total == 0 {
		return 0, os.ErrNotExist
	}
	return total, nil
}

type rootfsIdentity struct {
	dev uint64
	ino uint64
}

func markUniqueRootfs(info os.FileInfo, seen map[rootfsIdentity]struct{}) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return true
	}
	identity := rootfsIdentity{dev: uint64(stat.Dev), ino: uint64(stat.Ino)}
	if _, exists := seen[identity]; exists {
		return false
	}
	seen[identity] = struct{}{}
	return true
}
