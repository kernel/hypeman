package instances

import (
	"context"
	"errors"
	"path/filepath"
	"time"

	"github.com/kernel/hypeman/lib/logger"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

const (
	instanceTTLReaperInterval      = time.Minute
	instanceTTLReaperDeleteTimeout = 30 * time.Second
)

// StartTTLReaper deletes instances after their configured expiration time.
func (m *manager) StartTTLReaper(ctx context.Context) error {
	m.reapExpiredInstances(ctx)

	ticker := time.NewTicker(instanceTTLReaperInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			m.reapExpiredInstances(ctx)
		}
	}
}

func (m *manager) reapExpiredInstances(ctx context.Context) {
	log := logger.FromContext(ctx)
	metaFiles, err := m.listMetadataFiles()
	if err != nil {
		log.ErrorContext(ctx, "instance ttl reaper failed to list metadata", "error", err)
		return
	}

	now := m.nowUTC()
	for _, metaFile := range metaFiles {
		if ctx.Err() != nil {
			return
		}

		id := filepath.Base(filepath.Dir(metaFile))
		meta, err := m.loadMetadata(id)
		if errors.Is(err, ErrNotFound) {
			continue
		}
		if err != nil {
			log.ErrorContext(ctx, "instance ttl reaper failed to load metadata", "instance_id", id, "error", err)
			continue
		}
		if !instanceExpired(&meta.StoredMetadata, now) {
			continue
		}

		deleted, err := m.reapExpiredInstanceWithTimeout(ctx, id)

		switch {
		case err == nil && deleted:
			m.recordTTLReaperDeletion(ctx, "success")
		case err == nil:
			continue
		case errors.Is(err, context.DeadlineExceeded):
			m.recordTTLReaperDeletion(ctx, "timeout")
			log.ErrorContext(ctx, "instance ttl reaper timed out deleting instance", "instance_id", id, "error", err)
		case errors.Is(err, ErrNotFound):
			m.recordTTLReaperDeletion(ctx, "not_found")
		default:
			m.recordTTLReaperDeletion(ctx, "error")
			log.ErrorContext(ctx, "instance ttl reaper failed to delete instance", "instance_id", id, "error", err)
		}
	}
}

type ttlReaperDeleteResult struct {
	deleted bool
	err     error
}

func (m *manager) reapExpiredInstanceWithTimeout(ctx context.Context, id string) (bool, error) {
	deleteCtx, cancel := context.WithTimeout(ctx, m.instanceTTLReaperDeleteTimeout())
	defer cancel()

	resultCh := make(chan ttlReaperDeleteResult, 1)
	go func() {
		deleted, err := m.reapExpiredInstance(deleteCtx, id)
		resultCh <- ttlReaperDeleteResult{deleted: deleted, err: err}
	}()

	select {
	case result := <-resultCh:
		return result.deleted, result.err
	case <-deleteCtx.Done():
		return false, deleteCtx.Err()
	}
}

func (m *manager) reapExpiredInstance(ctx context.Context, id string) (bool, error) {
	lock := m.getInstanceLock(id)
	// Active lifecycle operations take precedence; retry on the next sweep.
	if !lock.TryLock() {
		return false, nil
	}
	defer lock.Unlock()

	meta, err := m.loadMetadata(id)
	if err != nil {
		return false, err
	}
	if !instanceExpired(&meta.StoredMetadata, m.nowUTC()) {
		return false, nil
	}
	return true, m.deleteInstanceLocked(ctx, id)
}

func (m *manager) instanceTTLReaperDeleteTimeout() time.Duration {
	if m.ttlReaperDeleteTimeout > 0 {
		return m.ttlReaperDeleteTimeout
	}
	return instanceTTLReaperDeleteTimeout
}

func instanceExpired(meta *StoredMetadata, now time.Time) bool {
	return meta != nil && meta.ExpiresAt != nil && !now.Before(*meta.ExpiresAt)
}

func (m *manager) recordTTLReaperDeletion(ctx context.Context, status string) {
	if m.metrics == nil {
		return
	}
	m.metrics.ttlReaperDeletionsTotal.Add(ctx, 1, metric.WithAttributes(attribute.String("status", status)))
}
