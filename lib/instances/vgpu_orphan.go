package instances

import (
	"context"
	"time"

	"github.com/kernel/hypeman/lib/logger"
)

const (
	// orphanedVGPUReleaseMaxAttempts bounds the retry loop so a genuinely
	// wedged VF degrades to one operator-actionable error instead of
	// indefinite log churn. At the default interval this covers ten minutes,
	// far beyond the seconds a dying VMM normally needs to finish kernel-side
	// VFIO teardown.
	orphanedVGPUReleaseMaxAttempts       = 20
	defaultOrphanedVGPUReleaseRetryDelay = 30 * time.Second
)

// scheduleOrphanedVGPURelease retries a vGPU release that failed during a
// completed delete, off the request path. Delete's log-and-continue contract
// is untouched — the caller already has its success — but without a retry the
// VF would stay allocated until the next startup reconciliation, silently
// shrinking host GPU capacity. A GPU-busy VMM routinely outlives delete's
// force-kill wait while the kernel finishes VFIO teardown, so this is the
// common case under load, not a tail case.
//
// Each attempt re-runs releaseStoredVGPU on a copy of the deleted instance's
// stored metadata, so the vendor VFIO claim scan and the destroy-side owner
// and open-handle guards apply on every retry exactly as they did on the
// original release. The queue is in-memory only: a restart abandons it and
// startup reconciliation sweeps the VF instead.
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
		if err := m.releaseStoredVGPU(ctx, &stored); err != nil {
			log.WarnContext(ctx, "orphaned vGPU release retry failed",
				"instance_id", stored.Id, "device_path", path, "attempt", attempt, "error", err)
			continue
		}
		log.InfoContext(ctx, "released orphaned vGPU after delete",
			"instance_id", stored.Id, "device_path", path, "attempt", attempt)
		return
	}
	log.ErrorContext(ctx, "giving up on orphaned vGPU release; VF stays allocated until startup reconciliation or manual remediation",
		"instance_id", stored.Id, "device_path", path, "attempts", orphanedVGPUReleaseMaxAttempts)
}
