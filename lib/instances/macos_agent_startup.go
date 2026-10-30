package instances

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"time"

	"github.com/kernel/hypeman/lib/instances/phasetracking"
	"github.com/kernel/hypeman/lib/logger"
)

// macOSAgentStartupDeadline bounds how long an agent-enabled macOS guest may stay
// Initializing. A missing or broken agent in the image would otherwise leave it
// there indefinitely.
const macOSAgentStartupDeadline = 10 * time.Minute

const macOSAgentStartupCheckInterval = 30 * time.Second

// StartMacOSAgentStartupController stops agent-enabled macOS guests whose agent did
// not become ready by the deadline, and records why in the exit message. Disks and
// logs are kept, and the restart policy applies as for any other stop.
func (m *manager) StartMacOSAgentStartupController(ctx context.Context) error {
	log := logger.FromContext(ctx).With("controller", "macos_agent_startup")
	log.InfoContext(ctx, "macOS agent startup controller started", "deadline", macOSAgentStartupDeadline)
	ticker := time.NewTicker(macOSAgentStartupCheckInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			m.enforceMacOSAgentStartupDeadline(ctx, log)
		}
	}
}

func (m *manager) enforceMacOSAgentStartupDeadline(ctx context.Context, log *slog.Logger) {
	files, err := m.listMetadataFiles()
	if err != nil {
		log.WarnContext(ctx, "list instances for macOS agent startup check", "error", err)
		return
	}
	for _, file := range files {
		id := filepath.Base(filepath.Dir(file))
		meta, err := m.loadMetadata(id)
		if err != nil || !macOSAgentStartupOverdue(&meta.StoredMetadata, m.nowUTC()) {
			continue
		}
		if err := m.stopOverdueMacOSAgentStartup(ctx, id, log); err != nil {
			log.WarnContext(ctx, "stop macOS guest whose agent did not start", "instance_id", id, "error", err)
		}
	}
}

// macOSAgentStartupOverdue reports whether an agent-enabled macOS guest has been
// Initializing for longer than the deadline. The clock is the current phase, so a
// guest resumed from a memory checkpoint gets a full deadline.
func macOSAgentStartupOverdue(stored *StoredMetadata, now time.Time) bool {
	return stored.MacOS != nil && stored.GuestAgentEnabled() && stored.GuestAgentReadyAt == nil &&
		stored.Phases.Current == phasetracking.PhaseInitializing && now.Sub(stored.Phases.Since) > macOSAgentStartupDeadline
}

func (m *manager) stopOverdueMacOSAgentStartup(ctx context.Context, id string, log *slog.Logger) error {
	lock := m.getInstanceLock(id)
	lock.Lock()
	defer lock.Unlock()

	// Probe once more under the lock; a ready agent is persisted and spared.
	m.persistBootMarkers(ctx, id)
	meta, err := m.loadMetadata(id)
	if err != nil || !macOSAgentStartupOverdue(&meta.StoredMetadata, m.nowUTC()) {
		return err
	}
	current := m.toInstanceWithoutHydration(ctx, meta)
	if current.State != StateInitializing {
		return nil
	}

	reason := fmt.Sprintf("guest agent did not become ready within %s; the image declares guest_agent", macOSAgentStartupDeadline)
	if last, ok := m.macOSAgentNotReady.Load(id); ok {
		reason += ": " + last.(string)
	}
	log.WarnContext(ctx, "stopping macOS guest whose agent did not start", "instance_id", id, "reason", reason)
	if _, err := m.stopInstance(ctx, id, false); err != nil {
		return err
	}
	meta, err = m.loadMetadata(id)
	if err != nil {
		return err
	}
	meta.ExitMessage = reason
	if err := m.saveMetadata(meta); err != nil {
		return err
	}
	m.macOSAgentNotReady.Delete(id)
	stopped := m.toInstanceWithoutHydration(ctx, meta)
	m.notifyLifecycleEvent(ctx, LifecycleEventStop, &stopped)
	return nil
}
