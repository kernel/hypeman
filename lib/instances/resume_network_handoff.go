package instances

import (
	"context"
	"fmt"

	"github.com/kernel/hypeman/lib/logger"
	"github.com/kernel/hypeman/lib/network"
)

type resumeNetworkHandoff struct {
	manager      *manager
	stored       *StoredMetadata
	allocatedNet *network.Allocation
	ackWaiter    *guestResumeNetworkUDPWaiter
	ackCfg       *guestNetworkConfig
	patched      bool
}

func (m *manager) prepareResumeNetworkHandoff(ctx context.Context, stored *StoredMetadata, allocatedNet *network.Allocation, snapshotDir string) (*resumeNetworkHandoff, error) {
	h := &resumeNetworkHandoff{
		manager:      m,
		stored:       stored,
		allocatedNet: allocatedNet,
	}
	if allocatedNet == nil || stored.SkipGuestAgent || !guestInitiatedResumeNetworkMailbox(stored) {
		return h, nil
	}

	log := logger.FromContext(ctx)
	resumeNetworkCfg, err := guestNetworkReconfigureConfig(allocatedNet)
	if err != nil {
		log.WarnContext(ctx, "failed to build guest resume network mailbox payload; falling back to host-initiated reconfigure", "instance_id", stored.Id, "error", err)
		return h, nil
	}

	waiter, err := startGuestResumeNetworkUDPWaiter()
	if err != nil {
		return nil, fmt.Errorf("start guest resume network UDP ack waiter: %w", err)
	}

	payload := newGuestResumeNetworkPayload(resumeNetworkCfg)
	payload.AckPort = waiter.Port()
	if err := patchGuestResumeNetworkMailbox(snapshotDir, guestInitiatedResumeNetworkMailboxToken(stored), &payload); err != nil {
		waiter.Close()
		log.WarnContext(ctx, "failed to patch guest resume network mailbox; falling back to host-initiated reconfigure", "instance_id", stored.Id, "error", err)
		return h, nil
	}

	h.ackWaiter = waiter
	h.ackCfg = resumeNetworkCfg
	h.patched = true
	return h, nil
}

func (h *resumeNetworkHandoff) Close() {
	if h != nil && h.ackWaiter != nil {
		h.ackWaiter.Close()
	}
}

func (h *resumeNetworkHandoff) AfterResume(ctx context.Context) error {
	if h == nil || h.allocatedNet == nil || h.stored.SkipGuestAgent {
		return nil
	}
	if h.patched {
		err := h.manager.waitForGuestResumeNetworkUDPAck(ctx, h.ackWaiter, h.stored, h.ackCfg)
		if err == nil {
			return nil
		}
		logger.FromContext(ctx).ErrorContext(ctx, "guest resume network UDP ack wait failed; falling back to host-initiated reconfigure", "instance_id", h.stored.Id, "error", err)
	}
	return reconfigureGuestNetwork(ctx, h.stored, h.allocatedNet)
}
