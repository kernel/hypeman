package instances

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"

	"github.com/kernel/hypeman/lib/devices"
	"github.com/kernel/hypeman/lib/logger"
)

// A fork can start using its source's passthrough device without changing
// AttachedTo. Don't let a release check liveness halfway through that boot.
func (m *manager) lockPCIDevices(ids []string) func() {
	ids = slices.Clone(ids)
	slices.Sort(ids)
	var locks []*sync.Mutex
	for _, id := range slices.Compact(ids) {
		v, _ := m.pciDeviceLocks.LoadOrStore(id, &sync.Mutex{})
		lock := v.(*sync.Mutex)
		lock.Lock()
		locks = append(locks, lock)
	}
	return func() {
		for _, lock := range locks {
			lock.Unlock()
		}
	}
}

func (m *manager) releasePCIDevices(ctx context.Context, stored *StoredMetadata) error {
	if len(stored.Devices) == 0 || m.deviceManager == nil {
		return nil
	}
	defer m.lockPCIDevices(stored.Devices)()
	log := logger.FromContext(ctx)
	for _, dev := range stored.Devices {
		err := m.deviceManager.ReleaseInstanceDevice(ctx, dev, stored.Id)
		if err == nil || errors.Is(err, devices.ErrNotFound) {
			continue
		}
		if stored.ForkPending != nil || errors.Is(err, devices.ErrInUse) {
			return fmt.Errorf("release PCI device %s: %w", dev, err)
		}
		log.WarnContext(ctx, "failed to release passthrough device", "instance_id", stored.Id, "device", dev, "error", err)
		// An ordinary delete can forget its attachment after a failed unbind,
		// but never when another guest may be using the device.
		if err := m.deviceManager.DetachInstanceDevice(ctx, dev, stored.Id); err != nil && !errors.Is(err, devices.ErrNotFound) {
			if errors.Is(err, devices.ErrInUse) {
				return err
			}
			log.WarnContext(ctx, "failed to detach passthrough device", "instance_id", stored.Id, "device", dev, "error", err)
		}
	}
	return nil
}
