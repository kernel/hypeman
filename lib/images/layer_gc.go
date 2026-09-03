package images

import (
	"context"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// layerEvictionGracePeriod keeps freshly written layer artifacts and temp
// directories out of cleanup so recovery and eviction never race builds that
// are still writing them.
const layerEvictionGracePeriod = 10 * time.Minute

// referencedLayerDigests returns the set of layer blob digests referenced by
// the manifest models of every image in the content layout, plus the digests
// currently referenced by in-flight builds. Layer artifacts in this set are
// protected from eviction. Unreadable manifest models are skipped with a
// warning so one corrupt record cannot disable eviction entirely.
func (m *manager) referencedLayerDigests() map[string]struct{} {
	refs := m.inflightLayerRefSnapshot()
	contentRoot := filepath.Join(m.paths.ImagesDir(), "content")
	err := filepath.WalkDir(contentRoot, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if entry.IsDir() || entry.Name() != "manifest.json" {
			return nil
		}
		digestHex := filepath.Base(filepath.Dir(path))
		model, readErr := readManifestModel(m.paths, digestHex)
		if readErr != nil {
			slog.Warn("skipping unreadable manifest model for layer eviction", "digest", digestHex, "error", readErr)
			return nil
		}
		if model == nil {
			return nil
		}
		for _, layer := range model.Layers {
			refs[strings.TrimPrefix(layer.Digest, "sha256:")] = struct{}{}
		}
		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		slog.Warn("failed to walk content manifests for layer eviction", "error", err)
	}
	return refs
}

// inflightLayerRefSnapshot returns the layer digests currently retained by
// in-flight builds.
func (m *manager) inflightLayerRefSnapshot() map[string]struct{} {
	m.layerRefMu.Lock()
	defer m.layerRefMu.Unlock()
	refs := make(map[string]struct{}, len(m.inflightLayerRefs))
	for digestHex := range m.inflightLayerRefs {
		refs[digestHex] = struct{}{}
	}
	return refs
}

// reconcileLayerStore evicts unreferenced layer artifacts and refreshes the
// cached disk usage totals so accounting reflects the removals.
func (m *manager) reconcileLayerStore() {
	m.createMu.Lock()
	defer m.createMu.Unlock()
	m.reconcileLayerStoreLocked()
}

// reconcileLayerStoreLocked is used by lifecycle operations that already hold
// createMu. Serializing reconciliation with manifest finalization prevents an
// eviction scan from racing a newly committed layer reference.
func (m *manager) reconcileLayerStoreLocked() {
	m.evictUnreferencedLayerArtifacts()
	m.refreshDiskUsageTotals()
}

// evictUnreferencedLayerArtifacts removes layer artifacts that no image
// manifest model references, deleting the digest directory entirely. Artifacts
// newer than the grace period are kept so in-flight builds never lose work.
func (m *manager) evictUnreferencedLayerArtifacts() {
	refs := m.referencedLayerDigests()

	layersDir := m.paths.ImageLayersDir()
	entries, err := os.ReadDir(layersDir)
	if err != nil {
		if !os.IsNotExist(err) {
			slog.Warn("layer eviction failed to list layer store", "error", err)
		}
		return
	}

	cutoff := time.Now().Add(-m.layerEvictionGrace)
	evicted := 0
	var evictedBytes int64
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		digestHex := entry.Name()
		if _, referenced := refs[digestHex]; referenced {
			continue
		}
		size, removed := m.tryEvictLayerArtifact(digestHex, filepath.Join(layersDir, digestHex), cutoff)
		if !removed {
			continue
		}
		evicted++
		evictedBytes += size
	}
	if evicted > 0 {
		slog.Info("evicted unreferenced layer artifacts", "count", evicted, "bytes", evictedBytes)
		if m.metrics != nil {
			m.metrics.layerArtifactsEvicted.Add(context.Background(), int64(evicted))
		}
	}
}

// tryEvictLayerArtifact removes one unreferenced layer artifact if it is still
// stale and no build is materializing it. Reconciliation holds createMu while
// scanning and eviction, so new builds cannot begin without being retained.
func (m *manager) tryEvictLayerArtifact(digestHex, dirPath string, cutoff time.Time) (int64, bool) {

	// The candidate was selected outside the lock; re-check that a build has
	// not retained the digest and the artifact has not been rewritten since.
	if _, referenced := m.inflightLayerRefSnapshot()[digestHex]; referenced {
		return 0, false
	}
	info, statErr := os.Stat(dirPath)
	if statErr != nil || info.ModTime().After(cutoff) {
		return 0, false
	}
	size, err := dirSize(dirPath)
	if err != nil {
		slog.Warn("failed to measure layer artifact size", "digest", digestHex, "error", err)
	}
	if err := os.RemoveAll(dirPath); err != nil {
		slog.Warn("failed to evict unreferenced layer artifact", "digest", digestHex, "error", err)
		return 0, false
	}
	return size, true
}

// cleanStaleImageTempDirs removes temp directories left behind by builds that
// were interrupted mid-install, mid-materialization, or mid-tag promotion.
// Only directories older than the grace period are removed so live builds are
// never disturbed.
func (m *manager) cleanStaleImageTempDirs() {
	roots := []string{
		m.paths.ImageLayersDir(),
		filepath.Join(m.paths.ImagesDir(), "content"),
	}
	cutoff := time.Now().Add(-m.layerEvictionGrace)
	for _, root := range roots {
		err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				if os.IsNotExist(err) {
					return nil
				}
				return err
			}
			if !entry.IsDir() {
				return nil
			}
			name := entry.Name()
			if !strings.HasPrefix(name, ".unpack-") && !strings.HasPrefix(name, ".install-") && !strings.HasPrefix(name, ".tag-stage-") {
				return nil
			}
			info, statErr := os.Stat(path)
			if statErr == nil && info.ModTime().Before(cutoff) {
				_ = os.RemoveAll(path)
			}
			return fs.SkipDir
		})
		if err != nil && !os.IsNotExist(err) {
			slog.Warn("failed to clean stale image temp dirs", "root", root, "error", err)
		}
	}
}

// totalLayerArtifactBytes sums the bytes held by materialized layer
// artifacts, matching what diskutilization.Collect counts for the same store.
func totalLayerArtifactBytes(layersDir string) (int64, error) {
	var total int64
	err := filepath.WalkDir(layersDir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if entry.IsDir() {
			return nil
		}
		if !strings.HasPrefix(entry.Name(), "layer.") {
			return nil
		}
		info, statErr := entry.Info()
		if statErr != nil {
			return nil
		}
		total += info.Size()
		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		return 0, fmt.Errorf("walk layer artifacts: %w", err)
	}
	return total, nil
}
