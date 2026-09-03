package images

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
)

// composeRootfs validates the persisted model and merges its layers into
// dest in manifest order, reading each layer blob from the shared OCI cache.
// Whiteout and opaque-directory markers are interpreted as each layer is
// applied. Any previous tree at dest is replaced: callers must not read dest
// concurrently, and a failure between the remove and the rename leaves dest
// absent.
func (c *ociClient) composeRootfs(ctx context.Context, dest, layoutTag string, model *imageManifestModel) error {
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
		if _, err := unpackCachedLayer(ctx, c.cacheBlobDir(), desc, staging, composeOnDiskFormat()); err != nil {
			return fmt.Errorf("apply layer %d (%s): %w", i, desc.Digest, err)
		}
	}
	// The export directory must stay traversable by other readers; MkdirTemp
	// creates it 0700.
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
