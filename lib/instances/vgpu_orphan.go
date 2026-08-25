package instances

import (
	"context"
	"time"

	"github.com/kernel/hypeman/lib/logger"
)

const (
	orphanedVGPUReleaseMaxAttempts       = 20
	defaultOrphanedVGPUReleaseRetryDelay = 30 * time.Second
)

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
