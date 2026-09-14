package images

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
)

// composeRootfs validates the persisted model and merges its layers into dest
// in manifest order. Whiteouts are applied to the composed tree.
func (c *ociClient) composeRootfs(ctx context.Context, dest, layoutTag string, model *imageManifestModel) error {
	if err := validateManifestModel(layoutTag, model); err != nil {
		return fmt.Errorf("validate manifest model: %w", err)
	}
	return c.composeLayerTree(ctx, dest, model.Layers)
}

func (c *ociClient) composeLayers(ctx context.Context, dest string, layers []layerDescriptor) error {
	return c.composeLayerTree(ctx, dest, layers)
}

func (c *ociClient) composeLayerTree(ctx context.Context, dest string, layers []layerDescriptor) error {
	parent := filepath.Dir(dest)
	if err := os.MkdirAll(parent, 0755); err != nil {
		return fmt.Errorf("create compose parent: %w", err)
	}
	leftovers, _ := filepath.Glob(filepath.Join(parent, ".compose-*"))
	for _, leftover := range leftovers {
		if err := removePath(leftover); err != nil {
			slog.Warn("failed to remove stale compose staging directory", "dir", leftover, "error", err)
		}
	}
	staging, err := os.MkdirTemp(parent, ".compose-*")
	if err != nil {
		return fmt.Errorf("create compose directory: %w", err)
	}
	defer func() {
		if err := removePath(staging); err != nil {
			slog.Warn("failed to remove compose staging directory", "dir", staging, "error", err)
		}
	}()
	if err := c.composeLayerList(ctx, staging, layers); err != nil {
		return err
	}
	if err := os.Chmod(staging, 0755); err != nil {
		return fmt.Errorf("set compose directory mode: %w", err)
	}
	if err := removePath(dest); err != nil {
		return fmt.Errorf("replace compose directory: %w", err)
	}
	if err := os.Rename(staging, dest); err != nil {
		return fmt.Errorf("install compose directory: %w", err)
	}
	return nil
}

func (c *ociClient) composeLayerList(ctx context.Context, dest string, layers []layerDescriptor) error {
	for i, desc := range layers {
		if _, err := unpackCachedLayer(ctx, c.cacheBlobDir(), desc, dest, composeOnDiskFormat()); err != nil {
			return fmt.Errorf("apply layer %d: %w", i, err)
		}
	}
	return nil
}
