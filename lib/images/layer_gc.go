package images

import (
	"context"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// layerEvictionGracePeriod keeps freshly written layer artifacts and temp
// directories out of cleanup so recovery and eviction never race builds that
// are still writing them.
const layerEvictionGracePeriod = 10 * time.Minute

// referencedLayerDigests returns the set of layer blob digests referenced by
// the manifest models of every image in the images tree — both content and
// legacy layouts write their model as a manifest.json with the digest as its
// parent directory — plus the digests currently referenced by in-flight
// builds. Layer artifacts in this set are protected from eviction. Unreadable
// manifest models are skipped with a warning so one corrupt record cannot
// disable eviction entirely.
//
// Callers must hold createMu so the in-flight map read is ordered with model
// writes during finalization.
func (m *manager) referencedLayerDigests() (map[string]struct{}, error) {
	refs := make(map[string]struct{}, len(m.inflightLayerRefs))
	for digestHex := range m.inflightLayerRefs {
		refs[digestHex] = struct{}{}
	}
	err := filepath.WalkDir(m.paths.ImagesDir(), func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			// An incomplete walk means an incomplete reference set; the caller
			// must not evict against it.
			return err
		}
		if entry.IsDir() || entry.Name() != "manifest.json" {
			return nil
		}
		digestHex := filepath.Base(filepath.Dir(path))
		model, readErr := readManifestModelAt(path, digestHex)
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
	if err != nil {
		return nil, fmt.Errorf("walk image manifests: %w", err)
	}
	return refs, nil
}

// inflightLayerRef is the handle returned by retainInflightLayers. Its
// release is idempotent: finalization releases the refs as soon as the
// manifest model is durable, and the build's deferred release becomes a
// no-op afterwards.
type inflightLayerRef struct {
	once        sync.Once
	digestHexes []string
}

func (r *inflightLayerRef) release(m *manager) {
	r.once.Do(func() {
		m.createMu.Lock()
		defer m.createMu.Unlock()
		m.releaseInflightLayerRefsLocked(r.digestHexes)
	})
}

// releaseLocked is release for callers already holding createMu, such as
// finalizeImage.
func (r *inflightLayerRef) releaseLocked(m *manager) {
	r.once.Do(func() { m.releaseInflightLayerRefsLocked(r.digestHexes) })
}

// retainInflightLayers registers one in-flight reference per digest so
// reconciliation cannot evict layers a build is materializing.
func (m *manager) retainInflightLayers(digestHexes []string) *inflightLayerRef {
	m.createMu.Lock()
	for _, digestHex := range digestHexes {
		m.inflightLayerRefs[digestHex]++
	}
	m.createMu.Unlock()
	return &inflightLayerRef{digestHexes: digestHexes}
}

func (m *manager) releaseInflightLayerRefsLocked(digestHexes []string) {
	for _, digestHex := range digestHexes {
		if m.inflightLayerRefs[digestHex] <= 1 {
			delete(m.inflightLayerRefs, digestHex)
		} else {
			m.inflightLayerRefs[digestHex]--
		}
	}
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
	refs, err := m.referencedLayerDigests()
	if err != nil {
		// Evicting against a truncated reference set would delete artifacts
		// belonging to images the walk never reached.
		slog.Warn("skipping layer eviction: incomplete reference scan", "error", err)
		return
	}

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
		m.recordLayerArtifactsEvicted(context.Background(), int64(evicted))
	}
}

// tryEvictLayerArtifact removes one unreferenced layer artifact if it is
// stale. Reconciliation holds createMu while scanning and eviction, and the
// reference set already contains the in-flight digests, so no build can
// register or drop a reference mid-pass.
func (m *manager) tryEvictLayerArtifact(digestHex, dirPath string, cutoff time.Time) (int64, bool) {
	info, statErr := os.Stat(dirPath)
	if statErr != nil || info.ModTime().After(cutoff) {
		return 0, false
	}
	size, err := dirSize(dirPath)
	if err != nil {
		slog.Warn("failed to measure layer artifact size", "digest", digestHex, "error", err)
	}
	// removePath clears read-only directories restored from layer metadata,
	// which os.RemoveAll cannot unlink through.
	if err := removePath(dirPath); err != nil {
		slog.Warn("failed to evict unreferenced layer artifact", "digest", digestHex, "error", err)
		return 0, false
	}
	return size, true
}

// isStaleTempDirName reports whether a directory name matches the temp
// prefixes builds use for staging, installs, and tag promotion.
func isStaleTempDirName(name string) bool {
	for _, prefix := range []string{".unpack-", ".install-", ".tag-stage-"} {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}

// cleanStaleImageTempDirs removes temp directories left behind by builds that
// were interrupted mid-install, mid-materialization, or mid-tag promotion.
// The walk covers the whole images tree: layer and content staging dirs plus
// .tag-stage-* dirs, which are created under images/<repository>/. Only
// directories older than the grace period are removed so live builds are
// never disturbed.
//
// This must stay a startup-only sweep: a staging dir's own mtime only moves
// when its direct children change, so a deep extraction running longer than
// the grace period can look stale while actively writing. A periodic sweep
// would need a heartbeat or a live-build registry first.
func (m *manager) cleanStaleImageTempDirs() {
	cutoff := time.Now().Add(-m.layerEvictionGrace)
	err := filepath.WalkDir(m.paths.ImagesDir(), func(path string, entry fs.DirEntry, err error) error {
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
		if !isStaleTempDirName(name) {
			return nil
		}
		info, statErr := os.Stat(path)
		if statErr == nil && info.ModTime().Before(cutoff) {
			if err := os.RemoveAll(path); err != nil {
				slog.Warn("failed to remove stale image temp dir", "dir", path, "error", err)
			}
		}
		return fs.SkipDir
	})
	if err != nil {
		slog.Warn("failed to clean stale image temp dirs", "root", m.paths.ImagesDir(), "error", err)
	}
}
