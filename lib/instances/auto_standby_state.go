package instances

import (
	"context"

	"github.com/kernel/hypeman/lib/autostandby"
)

// GetAutoStandbyState returns the persisted auto-standby state for an instance.
func (m *manager) GetAutoStandbyState(_ context.Context, id string) (*autostandby.AutoStandbyState, error) {
	lock := m.getInstanceLock(id)
	lock.Lock()
	defer lock.Unlock()

	meta, err := m.loadMetadata(id)
	if err != nil {
		return nil, err
	}
	return cloneAutoStandbyState(meta.AutoStandbyState), nil
}

// SetAutoStandbyState persists auto-standby state for an instance.
func (m *manager) SetAutoStandbyState(_ context.Context, id string, autoStandbyState *autostandby.AutoStandbyState) error {
	lock := m.getInstanceLock(id)
	lock.Lock()
	defer lock.Unlock()

	meta, err := m.loadMetadata(id)
	if err != nil {
		return err
	}
	meta.AutoStandbyState = cloneAutoStandbyState(autoStandbyState)
	return m.saveMetadata(meta)
}
