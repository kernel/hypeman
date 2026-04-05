package images

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// totalReadyImageBytesFromMetadata sums ready image sizes directly from metadata.json files.
// This is conservative for admission control and disk accounting: if metadata says an
// image is ready, we count its recorded size without re-validating the rootfs path.
func totalReadyImageBytesFromMetadata(imagesDir string) (int64, error) {
	var total int64

	err := filepath.Walk(imagesDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.IsDir() || info.Name() != "metadata.json" {
			return nil
		}

		data, err := os.ReadFile(path)
		if err != nil {
			return nil
		}

		var meta imageMetadata
		if err := json.Unmarshal(data, &meta); err != nil {
			return nil
		}
		if meta.Status == StatusReady && meta.SizeBytes > 0 {
			total += meta.SizeBytes
		}
		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		return 0, fmt.Errorf("walk images directory: %w", err)
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
			return nil
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
	return readyImageBytes, ociCacheBytes, nil
}
