package images

import (
	"fmt"
	"os"
)

// composeRootfs validates the persisted model and merges its layers into dest
// in manifest order, reading each layer blob from the shared OCI cache.
// Whiteout and opaque-directory markers are interpreted as each layer is
// applied.
func (c *ociClient) composeRootfs(dest, layoutTag string, model *imageManifestModel) error {
	if err := validateManifestModel(layoutTag, model); err != nil {
		return fmt.Errorf("validate manifest model: %w", err)
	}
	if len(model.Layers) == 0 {
		return fmt.Errorf("image has no layers")
	}
	if err := os.MkdirAll(dest, 0755); err != nil {
		return fmt.Errorf("create compose directory: %w", err)
	}
	for i, desc := range model.Layers {
		if err := c.applyLayerToDir(dest, desc); err != nil {
			return fmt.Errorf("apply layer %d (%s): %w", i, desc.Digest, err)
		}
	}
	return nil
}

func (c *ociClient) applyLayerToDir(dest string, desc layerDescriptor) error {
	layerDir, err := os.MkdirTemp("", "hypeman-layer-*")
	if err != nil {
		return fmt.Errorf("create layer staging directory: %w", err)
	}
	defer os.RemoveAll(layerDir)

	if _, err := unpackCachedLayer(c.cacheDir, desc, layerDir); err != nil {
		return err
	}
	if err := applyLayerTree(layerDir, dest); err != nil {
		return fmt.Errorf("apply layer tree: %w", err)
	}
	return nil
}
