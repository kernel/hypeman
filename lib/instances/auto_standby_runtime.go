package instances

import (
	"context"

	"github.com/kernel/hypeman/lib/autostandby"
)

// GetAutoStandbyRuntime returns the persisted auto-standby runtime metadata for an instance.
func (m *manager) GetAutoStandbyRuntime(_ context.Context, id string) (*autostandby.Runtime, error) {
	lock := m.getInstanceLock(id)
	lock.Lock()
	defer lock.Unlock()

	meta, err := m.loadMetadata(id)
	if err != nil {
		return nil, err
	}
	return cloneAutoStandbyRuntime(meta.AutoStandbyRuntime), nil
}

// SetAutoStandbyRuntime persists auto-standby runtime metadata for an instance.
func (m *manager) SetAutoStandbyRuntime(_ context.Context, id string, runtime *autostandby.Runtime) error {
	lock := m.getInstanceLock(id)
	lock.Lock()
	defer lock.Unlock()

	meta, err := m.loadMetadata(id)
	if err != nil {
		return err
	}
	meta.AutoStandbyRuntime = cloneAutoStandbyRuntime(runtime)
	return m.saveMetadata(meta)
}
