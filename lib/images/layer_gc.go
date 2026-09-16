package images

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// reconcileUnreferencedArtifacts removes materialized layer artifacts that are
// no longer referenced by a persisted image manifest. A running layer build
// suppresses the sweep because its manifest is not persisted until finalization.
func (s *layerStore) reconcileUnreferencedArtifacts(ctx context.Context) error {
	s.diskUsageMu.RLock()
	activeBuilds := s.activeBuilds
	s.diskUsageMu.RUnlock()
	if activeBuilds > 0 {
		return nil
	}

	live, err := referencedLayerDigests(ctx, s.paths.ImagesDir(), s.paths.ImageLayersDir())
	if err != nil {
		return err
	}
	entries, err := os.ReadDir(s.paths.ImageLayersDir())
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read layer artifact directory: %w", err)
	}
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		if _, ok := live[entry.Name()]; ok {
			continue
		}
		if err := removePath(filepath.Join(s.paths.ImageLayersDir(), entry.Name())); err != nil {
			return fmt.Errorf("remove unreferenced layer %s: %w", entry.Name(), err)
		}
		s.invalidateDiskUsageTotals()
	}
	return nil
}

func referencedLayerDigests(ctx context.Context, imagesDir, layersDir string) (map[string]struct{}, error) {
	live := make(map[string]struct{})
	err := walkWithContext(ctx, imagesDir, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			if os.IsNotExist(walkErr) {
				return nil
			}
			return walkErr
		}
		if info.IsDir() {
			if filepath.Clean(path) == filepath.Clean(layersDir) {
				return filepath.SkipDir
			}
			return nil
		}
		if info.Name() != "manifest.json" {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read manifest model %s: %w", path, err)
		}
		var model imageManifestModel
		if err := json.Unmarshal(data, &model); err != nil {
			return fmt.Errorf("unmarshal manifest model %s: %w", path, err)
		}
		for _, descriptor := range model.Layers {
			layerHex, err := layerDigestHex(descriptor.Digest)
			if err != nil {
				return fmt.Errorf("read layer reference from %s: %w", path, err)
			}
			live[layerHex] = struct{}{}
		}
		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("walk image manifests: %w", err)
	}
	return live, nil
}
