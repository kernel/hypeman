//go:build darwin

package instances

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/kernel/hypeman/lib/images"
)

// StopMacOSBuildInstance performs only the strict graceful stop used by build
// export. It copies no storage and never falls back to a forced hypervisor stop.
// Success requires the shutdown acknowledgement and confirmed VMM exit.
func (m *manager) StopMacOSBuildInstance(ctx context.Context, id string) (receipt MacOSBuildExportReceipt, err error) {
	if err = ctx.Err(); err != nil {
		return
	}
	if _, bounded := ctx.Deadline(); !bounded {
		return receipt, fmt.Errorf("machine stop requires a deadline")
	}
	lock := m.getInstanceLock(id)
	lock.Lock()
	defer lock.Unlock()
	current, err := m.currentInstanceWithoutHydration(ctx, id)
	if err != nil {
		return receipt, err
	}
	if current.MacOS == nil || !current.GuestAgentEnabled() || (current.State != StateRunning && current.State != StateInitializing) {
		return receipt, fmt.Errorf("strict stop requires an active macOS guest with a system agent")
	}
	if err = m.markRestartManualStopLocked(ctx, id); err != nil {
		return
	}
	stopped, err := m.stopInstance(ctx, id, true)
	if err != nil {
		return receipt, err
	}
	receipt = MacOSBuildExportReceipt{Graceful: true, VMMExited: true}
	m.notifyLifecycleEvent(ctx, LifecycleEventStop, stopped)
	return receipt, nil
}

// MacOSBuildStorage returns the closed storage of a Stopped macOS instance, checked
// under the instance lock. The caller publishes from these paths. A start in the
// meantime changes the overlay under the publisher, whose digest check then fails
// the upload instead of publishing it.
func (m *manager) MacOSBuildStorage(ctx context.Context, id string) (MacOSStorage, error) {
	if err := ctx.Err(); err != nil {
		return MacOSStorage{}, err
	}
	if _, bounded := ctx.Deadline(); !bounded {
		return MacOSStorage{}, fmt.Errorf("machine storage requires a deadline")
	}
	lock := m.getInstanceLock(id)
	lock.Lock()
	defer lock.Unlock()
	current, err := m.currentInstanceWithoutHydration(ctx, id)
	if err != nil {
		return MacOSStorage{}, err
	}
	if current.State != StateStopped || current.MacOS == nil {
		return MacOSStorage{}, fmt.Errorf("machine storage requires a stopped macOS instance")
	}
	storage := MacOSStorage{
		Layers:   macOSDiskParents(&current.StoredMetadata),
		Overlay:  m.paths.InstanceMacOSBootDisk(id),
		Aux:      filepath.Join(current.DataDir, macOSAuxStorageFile),
		Platform: *current.MacOS,
	}
	if err := checkClosedMacOSStorage(ctx, storage); err != nil {
		return MacOSStorage{}, err
	}
	return storage, nil
}

// checkClosedMacOSStorage requires every storage file to be a distinct nonempty
// regular file that no process holds open.
func checkClosedMacOSStorage(ctx context.Context, storage MacOSStorage) error {
	if err := storage.CheckFiles(); err != nil {
		return err
	}
	for _, path := range storage.Files() {
		if err := images.RequireClosedFile(ctx, path); err != nil {
			return err
		}
	}
	return nil
}
