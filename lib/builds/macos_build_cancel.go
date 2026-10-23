package builds

import (
	"context"
	"fmt"
	"sync"
	"time"
)

type machineBuildControl struct {
	mu     sync.Mutex
	cancel context.CancelFunc
}

func (m *manager) recordMachineBuilder(id, instanceID string) error {
	if active, ok := m.machineBuilds.Load(id); ok {
		control := active.(*machineBuildControl)
		control.mu.Lock()
		defer control.mu.Unlock()
	}
	meta, err := readMetadata(m.paths, id)
	if err != nil {
		return err
	}
	if meta.Status == StatusCancelled {
		return context.Canceled
	}
	meta.BuilderInstance = &instanceID
	return writeMetadata(m.paths, meta)
}

func (m *manager) cancelMachineBuild(id string) error {
	active, ok := m.machineBuilds.Load(id)
	if !ok {
		return ErrBuildInProgress
	}
	control := active.(*machineBuildControl)
	control.mu.Lock()
	defer control.mu.Unlock()
	meta, err := readMetadata(m.paths, id)
	if err != nil {
		return err
	}
	if meta.Status != StatusBuilding && meta.Status != StatusPushing {
		return fmt.Errorf("machine build no longer running")
	}
	// Persist cancellation before interrupting phases. The backend performs bounded
	// graceful cleanup or quarantine; Cancel must not force-delete mutable storage.
	meta.Status = StatusCancelled
	now := time.Now()
	meta.CompletedAt = &now
	if err := writeMetadata(m.paths, meta); err != nil {
		return err
	}
	control.cancel()
	m.notifyStatusChange(id, StatusCancelled)
	return nil
}
