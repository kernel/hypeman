package images

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/kernel/hypeman/lib/paths"
)

// stagedImageFiles are the build outputs written before finalization takes createMu.
// Finalization only renames them into place and commits metadata.
type stagedImageFiles struct {
	disk      string       // staged disk, beside its final path
	sizeBytes int64        // logical size of the disk, recorded for accounting
	macos     *stagedMacOS // nil for rootfs images
}

// remove deletes staged files that finalization did not install.
func (s stagedImageFiles) remove() {
	os.Remove(s.disk)
	if s.macos != nil {
		os.Remove(s.macos.aux)
	}
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
	diskPath := staged.finalDiskPath(layout)
	if err := installAtomically(diskPath, func(path string) error {
		return os.Rename(staged.disk, path)
	}); err != nil {
		return rollbackFinalization(installed, fmt.Errorf("install image disk: %w", err))
	}
	installed = append(installed, diskPath)

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
