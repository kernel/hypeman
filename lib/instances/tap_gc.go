package instances

import (
	"context"
	"time"

	"github.com/kernel/hypeman/lib/logger"
)

const (
	tapGCInterval = 60 * time.Second
	// tapGCMinAge must exceed the worst-case window between CreateAllocation creating
	// a TAP and saveMetadata persisting the instance to disk. Heavy work (image pull)
	// happens before CreateAllocation, so post-allocation work is bounded by volume
	// attach + config disk + VM boot — well under 5 minutes in practice.
	tapGCMinAge = 5 * time.Minute
)

// StartTAPGCReconciler starts a background goroutine that periodically deletes
// orphaned TAP devices on the host. Orphaned means: a hype-prefixed netdev whose
// instance ID does not correspond to any Running/Initializing/Unknown instance.
// To avoid racing against in-flight CreateAllocation calls whose metadata hasn't
// been persisted yet, only TAPs older than tapGCMinAge are considered.
// The goroutine stops when ctx is cancelled.
func (m *manager) StartTAPGCReconciler(ctx context.Context) {
	if ctx == nil {
		ctx = context.Background()
	}
	m.tapGCOnce.Do(func() {
		go func() {
			ticker := time.NewTicker(tapGCInterval)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					m.reconcileTAPs(ctx)
				}
			}
		}()
	})
}

func (m *manager) reconcileTAPs(ctx context.Context) {
	log := logger.FromContext(ctx)

	// Same preservation rules as startup (cmd/api/main.go): include Running,
	// Initializing, and Unknown. "Better to leave a stale TAP than crash a
	// running VM."
	insts, err := m.ListInstances(ctx, nil)
	var preserve []string
	if err != nil {
		log.WarnContext(ctx, "TAP GC: failed to list instances, skipping TAP pass", "error", err)
	} else {
		preserve = make([]string, 0, len(insts))
		for _, inst := range insts {
			if inst.State == StateRunning || inst.State == StateInitializing || inst.State == StateUnknown {
				preserve = append(preserve, inst.Id)
			}
		}
	}

	if err == nil {
		if len(preserve) == 0 {
			// CleanupOrphanedTAPs short-circuits on empty preserve set to avoid
			// clobbering TAPs from concurrent hypeman processes/tests. Mirror that here
			// rather than calling it with an empty slice.
			log.DebugContext(ctx, "TAP GC: no preserve candidates, skipping TAP pass")
		} else if deleted := m.networkManager.CleanupOrphanedTAPs(ctx, preserve, tapGCMinAge); deleted > 0 {
			log.InfoContext(ctx, "TAP GC: cleaned up orphaned TAP devices", "count", deleted)
		}
	}

	if deleted := m.networkManager.CleanupOrphanedClasses(ctx); deleted > 0 {
		log.InfoContext(ctx, "TAP GC: cleaned up orphaned tc filters/classes", "count", deleted)
	}
}
