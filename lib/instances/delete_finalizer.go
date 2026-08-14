package instances

import (
	"context"
	"errors"
	"path/filepath"
	"time"

	"github.com/kernel/hypeman/lib/logger"
)

// deleteFinalizeInterval is how often the delete finalizer retries teardown
// of pending-delete instances.
const deleteFinalizeInterval = 30 * time.Second

// StartDeleteFinalizer starts a background goroutine that finishes the
// teardown of instances whose delete was deferred. One pass runs immediately:
// after a host reboot the boot-scoped process identity proves a previously
// stuck hypervisor dead, so deferred deletes finalize right away. The
// goroutine stops when ctx is cancelled.
func (m *manager) StartDeleteFinalizer(ctx context.Context) {
	if ctx == nil {
		ctx = context.Background()
	}
	m.deleteFinalizerOnce.Do(func() {
		go func() {
			m.finalizePendingDeletes(ctx)
			ticker := time.NewTicker(deleteFinalizeInterval)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					m.finalizePendingDeletes(ctx)
				}
			}
		}()
	})
}

func (m *manager) finalizePendingDeletes(ctx context.Context) {
	log := logger.FromContext(ctx)
	ids, err := m.pendingDeleteInstanceIDs()
	if err != nil {
		log.WarnContext(ctx, "delete finalizer: failed to scan instance metadata", "error", err)
		return
	}
	for _, id := range ids {
		if ctx.Err() != nil {
			return
		}
		if err := m.finalizePendingDelete(ctx, id); err != nil {
			log.WarnContext(ctx, "delete finalizer: teardown still blocked", "instance_id", id, "error", err)
		}
	}
}

// finalizePendingDelete reruns the delete flow for one pending-delete
// instance. The rerun defers itself again while the hypervisor still cannot
// be confirmed dead, so a pass over a wedged instance is a log line rather
// than an error loop.
func (m *manager) finalizePendingDelete(ctx context.Context, id string) error {
	lock := m.getInstanceLock(id)
	lock.Lock()
	defer lock.Unlock()

	meta, err := m.loadMetadata(id)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil
		}
		return err
	}
	if meta.PendingDeleteAt == nil {
		return nil
	}
	if err := m.deleteInstanceWithOptions(ctx, id, deleteInstanceOptions{skipGracefulShutdown: true}); err != nil {
		return err
	}
	// The delete lifecycle event already fired when the delete was accepted;
	// only drop the state this instance still pins in memory.
	m.invalidateCachedHypervisorState(id)
	m.instanceLocks.Delete(id)
	logger.FromContext(ctx).InfoContext(ctx, "finalized deferred delete", "instance_id", id, "pending_since", meta.PendingDeleteAt)
	return nil
}

// pendingDeleteInstanceIDs returns the IDs of instances whose delete is
// deferred to the finalizer.
func (m *manager) pendingDeleteInstanceIDs() ([]string, error) {
	files, err := m.listMetadataFiles()
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, file := range files {
		id := filepath.Base(filepath.Dir(file))
		meta, err := m.loadMetadata(id)
		if err != nil {
			continue
		}
		if meta.PendingDeleteAt != nil {
			ids = append(ids, id)
		}
	}
	return ids, nil
}
