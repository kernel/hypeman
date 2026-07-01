package instances

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/kernel/hypeman/lib/hypervisor"
	"github.com/kernel/hypeman/lib/logger"
	"github.com/kernel/hypeman/lib/uffdpager"
)

// UFFDGraduationTargetVersion returns the pager version that new restores bind
// to, or "" when no UFFD pager is configured. The graduation controller treats
// sessions on a different version as the highest priority to retire.
func (m *manager) UFFDGraduationTargetVersion() string {
	if m == nil || m.firecrackerUFFDPager == nil {
		return ""
	}
	return m.firecrackerUFFDPager.VersionKey()
}

// GraduateSnapshotMemoryPager detaches a running UFFD-backed VM from its pager.
// The pager populates the remaining snapshot pages and unregisters the session,
// leaving the VM running on resident memory with no pager dependency. The VM is
// never paused and its network is untouched, so this is safe to run on a live
// VM that is serving traffic.
func (m *manager) GraduateSnapshotMemoryPager(ctx context.Context, id string) error {
	lock := m.getInstanceLock(id)
	lock.Lock()
	defer lock.Unlock()

	log := logger.FromContext(ctx)

	meta, err := m.loadMetadata(id)
	if err != nil {
		return err
	}
	stored := &meta.StoredMetadata

	if !m.snapshotMemoryPagerDetachable(stored.HypervisorType) {
		return fmt.Errorf("hypervisor %s does not use a detachable snapshot memory pager", stored.HypervisorType)
	}
	sessionID := strings.TrimSpace(stored.FirecrackerUFFDSessionID)
	if sessionID == "" {
		// Nothing bound to a pager; the VM is already independent.
		return nil
	}
	if m.firecrackerUFFDPager == nil {
		return fmt.Errorf("firecracker uffd pager is not configured")
	}

	inst := m.toInstanceWithoutHydration(ctx, meta)
	if inst.State != StateRunning {
		return fmt.Errorf("%w: cannot graduate snapshot memory pager from state %s", ErrInvalidState, inst.State)
	}

	version := strings.TrimSpace(stored.FirecrackerUFFDPagerVersion)
	if version == "" {
		// The version is stamped alongside the session ID at restore. Guessing a
		// version here could 404 against the wrong pager and clear the binding
		// while the real session is still alive on another one.
		return fmt.Errorf("uffd session %s has no recorded pager version", sessionID)
	}

	log.InfoContext(ctx, "graduating instance off uffd snapshot memory pager",
		"instance_id", id, "session_id", sessionID, "pager_version", version)

	err = m.firecrackerUFFDPager.CompleteSessionVersion(ctx, version, sessionID)
	if err != nil && !errors.Is(err, uffdpager.ErrSessionNotFound) {
		return fmt.Errorf("complete uffd session: %w", err)
	}

	// The session is gone whether we completed it or the pager had already lost
	// it, so clear all UFFD restore state, exactly like standby/stop do. This
	// includes the deferred snapshot memory path: memory is fully resident now,
	// so the next standby must write a self-contained Full snapshot instead of
	// materializing from the source snapshot (which may since be deleted).
	clearFirecrackerUFFDRestoreState(stored)
	meta = &metadata{StoredMetadata: *stored}
	if saveErr := m.saveMetadata(meta); saveErr != nil {
		return fmt.Errorf("save metadata after graduation: %w", saveErr)
	}
	return nil
}

func (m *manager) snapshotMemoryPagerDetachable(hvType hypervisor.Type) bool {
	caps, ok := hypervisor.CapabilitiesForType(hvType)
	if !ok {
		return false
	}
	return caps.UsesDetachableSnapshotMemoryPager
}
