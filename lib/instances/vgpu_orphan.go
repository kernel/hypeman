package instances

import (
	"context"
	"time"

	"github.com/kernel/hypeman/lib/logger"
)

const (
	// Bounds the retry loop (~10 minutes at the default interval, far beyond
	// normal VFIO teardown) so a wedged VF degrades to one operator-actionable
	// error instead of indefinite log churn.
	orphanedVGPUReleaseMaxAttempts       = 20
	defaultOrphanedVGPUReleaseRetryDelay = 30 * time.Second
)

// scheduleOrphanedVGPURelease retries a vGPU release for an assignment no
// on-disk metadata points at anymore: a release that failed during a
// completed delete (a GPU-busy VMM routinely outlives delete's force-kill
// wait), or a rollback whose retention record could not be saved. Without a
// record, nothing else releases the VF until startup reconciliation. Each
// attempt re-runs the release, so the claim scan and destroy guards apply on
// every retry. The queue is in-memory only; a restart abandons it and startup
// reconciliation sweeps the VF.
func (m *manager) scheduleOrphanedVGPURelease(ctx context.Context, stored StoredMetadata) {
	path := storedVGPUDevicePath(&stored)
	if path == "" {
		return
	}
	m.orphanedVGPUMu.Lock()
	if m.orphanedVGPUs == nil {
		m.orphanedVGPUs = make(map[string]struct{})
	}
	if _, pending := m.orphanedVGPUs[path]; pending {
		m.orphanedVGPUMu.Unlock()
		return
	}
	m.orphanedVGPUs[path] = struct{}{}
	m.orphanedVGPUMu.Unlock()

	delay := m.orphanedVGPURetryDelay
	if delay <= 0 {
		delay = defaultOrphanedVGPUReleaseRetryDelay
	}
	// The request context ends with the delete; keep its values for logging
	// but detach from its cancellation.
	go m.retryOrphanedVGPURelease(context.WithoutCancel(ctx), stored, path, delay)
}

func (m *manager) retryOrphanedVGPURelease(ctx context.Context, stored StoredMetadata, path string, delay time.Duration) {
	log := logger.FromContext(ctx)
	defer func() {
		m.orphanedVGPUMu.Lock()
		delete(m.orphanedVGPUs, path)
		m.orphanedVGPUMu.Unlock()
	}()
	for attempt := 1; attempt <= orphanedVGPUReleaseMaxAttempts; attempt++ {
		time.Sleep(delay)
		// No claim-scan exclusion: unlike delete, a failed start keeps its
		// instance record, and a restarted instance may hold this same VF.
		if err := m.releaseStoredVGPUExcluding(ctx, &stored, ""); err != nil {
			log.WarnContext(ctx, "orphaned vGPU release retry failed",
				"instance_id", stored.Id, "device_path", path, "attempt", attempt, "error", err)
			continue
		}
		log.InfoContext(ctx, "released orphaned vGPU after delete",
			"instance_id", stored.Id, "device_path", path, "attempt", attempt)
		return
	}
	m.recordVGPUOrphanReleaseAbandoned(ctx)
	log.ErrorContext(ctx, "giving up on orphaned vGPU release; VF stays allocated until startup reconciliation or manual remediation",
		"instance_id", stored.Id, "device_path", path, "attempts", orphanedVGPUReleaseMaxAttempts)
}
