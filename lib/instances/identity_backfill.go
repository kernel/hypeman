package instances

import (
	"context"
	"path/filepath"

	"github.com/kernel/hypeman/lib/logger"
)

// BackfillHypervisorProcessIdentities persists process identity for instances
// recorded before identity tokens existed, avoiding full /proc scans during
// later state derivation.
func (m *manager) BackfillHypervisorProcessIdentities(ctx context.Context) {
	log := logger.FromContext(ctx)

	files, err := m.listMetadataFiles()
	if err != nil {
		log.WarnContext(ctx, "failed to list instances for hypervisor identity backfill", "error", err)
		return
	}

	for _, file := range files {
		id := filepath.Base(filepath.Dir(file))
		meta, err := m.loadMetadata(id)
		if err != nil {
			continue
		}
		if !needsHypervisorIdentityBackfill(&meta.StoredMetadata) {
			continue
		}
		m.backfillInstanceHypervisorProcessIdentity(ctx, id)
	}
}

func needsHypervisorIdentityBackfill(stored *StoredMetadata) bool {
	if stored == nil || stored.HypervisorPID == nil || stored.SocketPath == "" {
		return false
	}
	if stored.HypervisorStartTime != 0 && stored.HypervisorBootID != "" {
		return false
	}
	return ProcessExists(*stored.HypervisorPID)
}

func (m *manager) backfillInstanceHypervisorProcessIdentity(ctx context.Context, id string) {
	log := logger.FromContext(ctx)
	lock := m.getInstanceLock(id)
	lock.Lock()
	defer lock.Unlock()

	meta, err := m.loadMetadata(id)
	if err != nil {
		return
	}
	if !needsHypervisorIdentityBackfill(&meta.StoredMetadata) {
		return
	}

	pid, err := resolveLiveHypervisorPID(meta.HypervisorProcessIdentity, meta.SocketPath)
	if err != nil || pid <= 0 {
		log.DebugContext(ctx, "skipping hypervisor identity backfill", "instance_id", id, "error", err)
		return
	}

	meta.HypervisorProcessIdentity.Set(pid)
	if err := m.saveMetadata(meta); err != nil {
		log.WarnContext(ctx, "failed to persist hypervisor process identity", "instance_id", id, "error", err)
		return
	}
	log.DebugContext(ctx, "persisted hypervisor process identity", "instance_id", id, "pid", pid)
}
