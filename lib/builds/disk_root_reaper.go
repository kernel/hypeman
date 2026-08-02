package builds

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/kernel/hypeman/lib/volumes"
)

// diskRootReaperInterval is how often the reaper sweeps for orphaned
// BuildKit root volumes after the initial startup sweep.
const diskRootReaperInterval = 15 * time.Minute

// diskRootVolumePrefix prefixes the per-build BuildKit root volume ID.
const diskRootVolumePrefix = "build-disk-"

// runDiskRootReaper deletes orphaned build-disk-* volumes left behind when
// the process crashes mid-build and the cleanup defer in executeBuild never
// runs. It sweeps once at startup and then periodically until ctx is
// cancelled. It runs regardless of DiskRootEnabled because orphans may exist
// from when the feature was enabled.
func (m *manager) runDiskRootReaper(ctx context.Context) {
	m.sweepOrphanedDiskRootVolumes(ctx)

	ticker := time.NewTicker(diskRootReaperInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.sweepOrphanedDiskRootVolumes(ctx)
		}
	}
}

// sweepOrphanedDiskRootVolumes deletes build-disk-* volumes whose build is
// gone or in a terminal state. Volumes for active builds and races against
// concurrent use (ErrInUse/ErrNotFound) are skipped.
func (m *manager) sweepOrphanedDiskRootVolumes(ctx context.Context) {
	vols, err := m.volumeManager.ListVolumes(ctx)
	if err != nil {
		m.logger.Error("list volumes for buildkit root volume reaper", "error", err)
		return
	}

	reaped := 0
	for _, vol := range vols {
		buildID, ok := strings.CutPrefix(vol.Id, diskRootVolumePrefix)
		if !ok || buildID == "" {
			continue
		}

		if !m.buildTerminalOrGone(buildID) {
			continue
		}

		err := m.volumeManager.DeleteVolume(ctx, vol.Id)
		switch {
		case err == nil:
			reaped++
			m.logger.Info("reaped orphaned buildkit root volume", "build_id", buildID, "volume_id", vol.Id)
		case errors.Is(err, volumes.ErrInUse), errors.Is(err, volumes.ErrNotFound):
			m.logger.Debug("skipping buildkit root volume", "build_id", buildID, "volume_id", vol.Id, "reason", err)
		default:
			m.logger.Warn("failed to delete orphaned buildkit root volume", "build_id", buildID, "volume_id", vol.Id, "error", err)
		}
	}

	if reaped > 0 {
		m.logger.Info("reaped orphaned buildkit root volumes", "count", reaped)
	}
}

// buildTerminalOrGone reports whether the build is safe to reap resources
// for: its metadata is missing or unreadable, or its status is terminal.
func (m *manager) buildTerminalOrGone(buildID string) bool {
	meta, err := readMetadata(m.paths, buildID)
	if err != nil {
		m.logger.Debug("build metadata unreadable, treating volume as orphaned", "build_id", buildID, "error", err)
		return true
	}
	switch meta.Status {
	case StatusReady, StatusFailed, StatusCancelled:
		return true
	default:
		return false
	}
}
