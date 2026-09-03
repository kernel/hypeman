package images

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
)

// composeRootfs validates the persisted model and merges its layers into dest
// in manifest order, reading each layer blob from the shared OCI cache.
// Whiteout and opaque-directory markers are interpreted as each layer is
// applied.
func (c *ociClient) composeRootfs(dest, layoutTag string, model *imageManifestModel) error {
	return c.composeRootfsContext(context.Background(), dest, layoutTag, model)
}

func (c *ociClient) composeRootfsContext(ctx context.Context, dest, layoutTag string, model *imageManifestModel) error {
	if err := validateManifestModel(layoutTag, model); err != nil {
		return fmt.Errorf("validate manifest model: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0755); err != nil {
		return fmt.Errorf("create compose parent: %w", err)
	}
	staging, err := os.MkdirTemp(filepath.Dir(dest), ".compose-*")
	if err != nil {
		return fmt.Errorf("create compose directory: %w", err)
	}
	defer os.RemoveAll(staging)

	for i, desc := range model.Layers {
		if err := c.applyLayerToDir(ctx, staging, desc); err != nil {
			return fmt.Errorf("apply layer %d (%s): %w", i, desc.Digest, err)
		}
	}
	if err := os.RemoveAll(dest); err != nil {
		return fmt.Errorf("replace compose directory: %w", err)
	}
	if err := os.Rename(staging, dest); err != nil {
		return fmt.Errorf("install compose directory: %w", err)
	}
	return nil
}

func (c *ociClient) applyLayerToDir(ctx context.Context, dest string, desc layerDescriptor) error {
	layerDir, err := os.MkdirTemp(filepath.Dir(dest), ".layer-*")
	if err != nil {
		return fmt.Errorf("create layer staging directory: %w", err)
	}
	defer os.RemoveAll(layerDir)

	stats, err := unpackCachedLayerContext(ctx, c.cacheDir, desc, layerDir)
	if err != nil {
		return err
	}
	if err := applyLayerTreeWithExplicitDirs(layerDir, dest, stats.explicitDirs); err != nil {
		return fmt.Errorf("apply layer tree: %w", err)
	}
	return nil
}
