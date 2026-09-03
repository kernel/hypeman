package images

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
)

// composeRootfsContext validates the persisted model and merges its layers
// into dest in manifest order, reading each layer blob from the shared OCI
// cache. Whiteout and opaque-directory markers are interpreted as each layer
// is applied.
func (c *ociClient) composeRootfsContext(ctx context.Context, dest, layoutTag string, model *imageManifestModel) error {
	if err := validateManifestModel(layoutTag, model); err != nil {
		return fmt.Errorf("validate manifest model: %w", err)
	}
	parent := filepath.Dir(dest)
	if err := os.MkdirAll(parent, 0755); err != nil {
		return fmt.Errorf("create compose parent: %w", err)
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

	for i, desc := range model.Layers {
		if _, err := unpackCachedLayer(ctx, filepath.Join(c.cacheDir, "blobs", "sha256"), desc, staging, composeOnDiskFormat()); err != nil {
			return fmt.Errorf("apply layer %d (%s): %w", i, desc.Digest, err)
		}
	}
	// Match the export-directory mode the unpack path used; MkdirTemp is 0700.
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
