package images

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// totalReadyImageBytesFromMetadata sums ready image sizes directly from metadata.json files.
// This is conservative for admission control and disk accounting: if metadata says an
// image is ready, we count its recorded size without re-validating the rootfs path. If
// the metadata file is unreadable or malformed, we fall back to counting any rootfs disk
// files found in the digest directory so we do not undercount host disk usage.
func totalReadyImageBytesFromMetadata(imagesDir string) (int64, error) {
	var total int64
	seenRootfs := make(map[rootfsIdentity]struct{})

	err := filepath.Walk(imagesDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if info.IsDir() || info.Name() != "metadata.json" {
			return nil
		}

		data, err := os.ReadFile(path)
		if err != nil {
			rootfsBytes, fallbackErr := totalUniqueRootfsBytesInDigestDir(filepath.Dir(path), seenRootfs)
			if fallbackErr == nil {
				total += rootfsBytes
				return nil
			}
			return fmt.Errorf("read image metadata %s: %w", path, err)
		}

		var meta imageMetadata
		if err := json.Unmarshal(data, &meta); err != nil {
			rootfsBytes, fallbackErr := totalUniqueRootfsBytesInDigestDir(filepath.Dir(path), seenRootfs)
			if fallbackErr == nil {
				total += rootfsBytes
				return nil
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
			rootfsBytes, err := totalRootfsBytesInDigestDir(filepath.Dir(path))
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

// totalLayerArtifactBytesFromFilesystem sums materialized layer artifacts.
func totalLayerArtifactBytesFromFilesystem(layersDir string) (int64, error) {
	var total int64
	err := filepath.WalkDir(layersDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if d.IsDir() {
			if strings.HasPrefix(d.Name(), ".") && path != layersDir {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasPrefix(d.Name(), "layer.") {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		total += info.Size()
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("walk layer artifacts: %w", err)
	}
	return total, nil
}

// totalOCICacheBlobBytesFromFilesystem sums blob sizes directly from the OCI cache blob store.
// This counts the actual bytes on disk, including any blob files that are currently
// present but no longer referenced by the OCI layout index.
func totalOCICacheBlobBytesFromFilesystem(blobDir string) (int64, error) {
	var total int64

	err := filepath.Walk(blobDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if info.IsDir() {
			return nil
		}
		total += info.Size()
		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		return 0, fmt.Errorf("walk OCI cache blobs: %w", err)
	}

	return total, nil
}

func (m *manager) getDiskUsageTotals() (int64, int64, error) {
	m.diskUsageMu.RLock()
	if m.diskUsageLoaded {
		readyImageBytes := m.readyImageBytes
		ociCacheBytes := m.ociCacheBytes
		m.diskUsageMu.RUnlock()
		return readyImageBytes, ociCacheBytes, nil
	}
	m.diskUsageMu.RUnlock()

	readyImageBytes, ociCacheBytes, err := m.computeDiskUsageTotals()
	if err != nil {
		return 0, 0, err
	}

	m.diskUsageMu.Lock()
	if !m.diskUsageLoaded {
		m.readyImageBytes = readyImageBytes
		m.ociCacheBytes = ociCacheBytes
		m.diskUsageLoaded = true
	}
	readyImageBytes = m.readyImageBytes
	ociCacheBytes = m.ociCacheBytes
	m.diskUsageMu.Unlock()

	return readyImageBytes, ociCacheBytes, nil
}

func (m *manager) refreshDiskUsageTotals() {
	readyImageBytes, ociCacheBytes, err := m.computeDiskUsageTotals()
	if err != nil {
		return
	}

	m.diskUsageMu.Lock()
	m.readyImageBytes = readyImageBytes
	m.ociCacheBytes = ociCacheBytes
	m.diskUsageLoaded = true
	m.diskUsageMu.Unlock()
}

func (m *manager) computeDiskUsageTotals() (int64, int64, error) {
	readyImageBytes, err := totalReadyImageBytesFromMetadata(m.paths.ImagesDir())
	if err != nil {
		return 0, 0, err
	}
	ociCacheBytes, err := totalOCICacheBlobBytesFromFilesystem(m.paths.OCICacheBlobDir())
	if err != nil {
		return 0, 0, err
	}
	layerArtifactBytes, err := totalLayerArtifactBytesFromFilesystem(m.paths.ImageLayersDir())
	if err != nil {
		return 0, 0, err
	}
	return readyImageBytes, ociCacheBytes + layerArtifactBytes, nil
}

func totalRootfsBytesInDigestDir(digestDir string) (int64, error) {
	rootfsPaths, err := filepath.Glob(filepath.Join(digestDir, "rootfs.*"))
	if err != nil {
		return 0, err
	}
	if len(rootfsPaths) == 0 {
		return 0, os.ErrNotExist
	}

	var total int64
	for _, rootfsPath := range rootfsPaths {
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
		total += info.Size()
	}
	if total == 0 {
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

func totalUniqueRootfsBytesInDigestDir(digestDir string, seen map[rootfsIdentity]struct{}) (int64, error) {
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
		if !markUniqueRootfs(info, seen) {
			continue
		}
		total += info.Size()
	}
	if !found {
		return 0, os.ErrNotExist
	}
	return total, nil
}
