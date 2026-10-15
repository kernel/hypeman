package images

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/kernel/hypeman/lib/paths"
)

// stagedImageFiles are the build outputs written before finalization takes createMu.
// Finalization only renames them into place and commits metadata.
type stagedImageFiles struct {
	disk      string      // staged disk, beside its final path
	aux       string      // staged auxiliary storage; macOS machine images only
	macos     *MacOSImage // platform of a macOS machine image; nil for rootfs images
	sizeBytes int64       // bytes the staged files occupy, recorded for accounting
}

func (m *manager) finalizeImage(ref *ResolvedRef, result *pullResult, buildID string, staged stagedImageFiles) error {
	m.createMu.Lock()
	defer m.createMu.Unlock()

	layout := resolveImageLayout(m.paths, ref.Repository(), ref.DigestHex())

	// Read current metadata to preserve request info and reject stale builds.
	meta, err := readMetadataAt(layout)
	if err != nil || meta.BuildID != buildID {
		return errStaleBuild
	}

	// The pulled image config is the source of truth for the platform.
	var requestedPlatform string
	if meta.Request != nil {
		requestedPlatform = meta.Request.Platform
	}
	actualPlatform, err := resolveManifestPlatform(result.Metadata, requestedPlatform)
	if err != nil {
		return err
	}

	// Files installed before the metadata commit. Finalization failure removes them.
	installed, err := installMacOSMachine(layout, staged, meta, actualPlatform)
	if err != nil {
		return rollbackFinalization(installed, err)
	}
	if err := installAtomically(layout.disk, func(path string) error {
		return os.Rename(staged.disk, path)
	}); err != nil {
		return rollbackFinalization(installed, fmt.Errorf("install image disk: %w", err))
	}
	installed = append(installed, layout.disk)

	// Persist the manifest content model beside the shared content so later
	// stages can recompose the image from per-layer artifacts and GC can tell
	// which OCI blobs are still referenced.
	if result.Manifest != nil {
		model := *result.Manifest
		model.Platform = actualPlatform.String()
		modelPath := manifestModelPath(m.paths, layout, ref.DigestHex())
		if err := writeManifestModelAt(modelPath, ref.DigestHex(), &model); err != nil {
			return rollbackFinalization(installed, fmt.Errorf("write manifest model: %w", err))
		}
		installed = append(installed, modelPath)
	}

	meta.Status = StatusReady
	meta.Error = nil
	meta.Platform = actualPlatform.String()
	meta.SizeBytes = staged.sizeBytes
	meta.Entrypoint = result.Metadata.Entrypoint
	meta.Cmd = result.Metadata.Cmd
	meta.Env = result.Metadata.Env
	meta.Labels = result.Metadata.Labels
	meta.WorkingDir = result.Metadata.WorkingDir

	if err := writeMetadataFile(layout.metadata, meta); err != nil {
		return rollbackFinalization(installed, fmt.Errorf("write final metadata: %w", err))
	}

	m.notifyReady(ref.DigestHex(), StatusReady, nil)
	if !m.claimRequestedTags(ref, meta) {
		m.cleanupUnclaimedImage(ref)
	}
	m.layers.refreshDiskUsageTotals()
	return nil
}

func manifestModelPath(p *paths.Paths, layout imageLayout, digestHex string) string {
	if layout.content {
		return p.ImageContentManifestModel(digestHex)
	}
	return filepath.Join(layout.dir, "manifest.json")
}

// rollbackFinalization removes the files finalization installed before it failed.
func rollbackFinalization(installed []string, cause error) error {
	var rollbackErr error
	for _, path := range installed {
		rollbackErr = errors.Join(rollbackErr, os.Remove(path))
	}
	if rollbackErr != nil {
		return errors.Join(cause, fmt.Errorf("rollback finalization: %w", rollbackErr))
	}
	return cause
}

// stageBuildOutputs writes a pulled image's build outputs beside their final paths:
// an exported rootfs disk, or a macOS machine image's disk and auxiliary storage.
// The returned cleanup removes staged files that finalization did not install.
func (m *manager) stageBuildOutputs(ctx context.Context, digest, tempDir string, meta *containerMetadata, layoutDir, diskTempPath, buildID string) (stagedImageFiles, func(), error) {
	cleanup := func() {}
	payload, err := parseMacOSMachine(tempDir, meta)
	if err != nil {
		return stagedImageFiles{}, cleanup, err
	}
	convertStart := time.Now()
	var staged stagedImageFiles
	if payload != nil {
		auxTemp := filepath.Join(layoutDir, "aux.img.tmp-"+buildID)
		cleanup = func() { os.Remove(auxTemp) }
		staged, err = stageMacOSMachine(payload, diskTempPath, auxTemp)
	} else {
		// Use default image format (erofs on Linux, ext4 on Darwin)
		staged = stagedImageFiles{disk: diskTempPath}
		staged.sizeBytes, err = ExportRootfs(tempDir, diskTempPath, DefaultImageFormat)
	}
	m.recordImageBuildPhase(ctx, digest, "filesystem_export", time.Since(convertStart), phaseStatus(err), "not_applicable")
	if err != nil {
		return staged, cleanup, fmt.Errorf("convert to %s: %w", DefaultImageFormat, err)
	}
	return staged, cleanup, nil
}
