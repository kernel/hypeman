package instances

import (
	"context"
	"testing"
	"time"

	"github.com/kernel/hypeman/lib/hypervisor"
)

func TestAcquireFirecrackerRestoreSlotLimitsFirecrackerOnly(t *testing.T) {
	m := &manager{firecrackerRestoreSlots: make(chan struct{}, 1)}
	stored := &StoredMetadata{HypervisorType: hypervisor.TypeFirecracker}

	release, err := m.acquireFirecrackerRestoreSlot(context.Background(), stored)
	if err != nil {
		t.Fatalf("first acquire failed: %v", err)
	}
	defer release()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if _, err := m.acquireFirecrackerRestoreSlot(ctx, stored); err == nil {
		t.Fatalf("expected second firecracker acquire to wait until context expires")
	}

	if releaseOther, err := m.acquireFirecrackerRestoreSlot(context.Background(), &StoredMetadata{HypervisorType: hypervisor.TypeCloudHypervisor}); err != nil {
		t.Fatalf("non-firecracker acquire should not wait: %v", err)
	} else {
		releaseOther()
	}
}

func TestFirecrackerRestoreSlotsSharedByLimit(t *testing.T) {
	stored := &StoredMetadata{HypervisorType: hypervisor.TypeFirecracker}
	slots := sharedFirecrackerRestoreSlots(1)
	m1 := &manager{firecrackerRestoreSlots: slots}
	m2 := &manager{firecrackerRestoreSlots: slots}

	release, err := m1.acquireFirecrackerRestoreSlot(context.Background(), stored)
	if err != nil {
		t.Fatalf("first acquire failed: %v", err)
	}
	defer release()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if _, err := m2.acquireFirecrackerRestoreSlot(ctx, stored); err == nil {
		t.Fatalf("expected second manager to share restore capacity")
	}
}
