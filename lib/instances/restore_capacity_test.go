package instances

import (
	"context"
	"testing"
	"time"

	"github.com/kernel/hypeman/lib/hypervisor"
)

func TestAcquireRestoreSlotLimitsConfiguredHypervisorOnly(t *testing.T) {
	m := &manager{restoreSlotsByHypervisor: map[hypervisor.Type]chan struct{}{
		hypervisor.TypeFirecracker: make(chan struct{}, 1),
	}}

	release, err := m.acquireRestoreSlot(context.Background(), hypervisor.TypeFirecracker)
	if err != nil {
		t.Fatalf("first acquire failed: %v", err)
	}
	defer release()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if _, err := m.acquireRestoreSlot(ctx, hypervisor.TypeFirecracker); err == nil {
		t.Fatalf("expected second firecracker acquire to wait until context expires")
	}

	if releaseOther, err := m.acquireRestoreSlot(context.Background(), hypervisor.TypeCloudHypervisor); err != nil {
		t.Fatalf("unconfigured hypervisor acquire should not wait: %v", err)
	} else {
		releaseOther()
	}
}

func TestRestoreSlotsSharedByHypervisorAndLimit(t *testing.T) {
	slots := sharedRestoreSlots(hypervisor.TypeFirecracker, 1)
	m1 := &manager{restoreSlotsByHypervisor: map[hypervisor.Type]chan struct{}{
		hypervisor.TypeFirecracker: slots,
	}}
	m2 := &manager{restoreSlotsByHypervisor: map[hypervisor.Type]chan struct{}{
		hypervisor.TypeFirecracker: slots,
	}}

	release, err := m1.acquireRestoreSlot(context.Background(), hypervisor.TypeFirecracker)
	if err != nil {
		t.Fatalf("first acquire failed: %v", err)
	}
	defer release()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if _, err := m2.acquireRestoreSlot(ctx, hypervisor.TypeFirecracker); err == nil {
		t.Fatalf("expected second manager to share restore capacity")
	}
}
