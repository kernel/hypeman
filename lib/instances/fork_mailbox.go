package instances

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/kernel/hypeman/lib/hypervisor"
	"github.com/kernel/hypeman/lib/logger"
	mailboxpkg "github.com/kernel/hypeman/lib/mailbox"
	"go.opentelemetry.io/otel/attribute"
)

var forkMailboxOffsetByMark sync.Map

type patchedForkMailbox struct {
	name    string
	waiter  *guestResumeNetworkUDPWaiter
	timeout time.Duration
}

type forkMailboxPatch struct {
	name    string
	token   string
	payload []byte
}

type forkMailboxHandoff struct {
	manager *manager
	stored  *StoredMetadata
	patched []patchedForkMailbox
}

func (m *manager) prepareForkMailboxHandoff(ctx context.Context, stored *StoredMetadata, snapshotDir string, mailboxes []ForkMailboxPayload) (*forkMailboxHandoff, error) {
	patched, err := m.patchForkMailboxes(ctx, stored, snapshotDir, mailboxes)
	if err != nil {
		return nil, err
	}
	return &forkMailboxHandoff{
		manager: m,
		stored:  stored,
		patched: patched,
	}, nil
}

func (h *forkMailboxHandoff) AfterResume(ctx context.Context) error {
	if h == nil {
		return nil
	}
	return h.manager.waitForForkMailboxAcks(ctx, h.stored, h.patched)
}

func (h *forkMailboxHandoff) Close() {
	if h == nil {
		return
	}
	closePatchedForkMailboxes(h.patched)
}

func validateForkMailboxes(mailboxes []ForkMailboxPayload) error {
	if len(mailboxes) > 16 {
		return fmt.Errorf("%w: at most 16 mailboxes can be patched for a fork", ErrInvalidRequest)
	}
	seen := make(map[string]struct{}, len(mailboxes))
	for _, mailbox := range mailboxes {
		name := mailbox.Name
		if !mailboxpkg.ValidForkMailboxName(name) {
			return fmt.Errorf("%w: invalid mailbox name %q", ErrInvalidRequest, mailbox.Name)
		}
		if _, ok := seen[name]; ok {
			return fmt.Errorf("%w: duplicate mailbox name %q", ErrInvalidRequest, name)
		}
		seen[name] = struct{}{}

		if strings.TrimSpace(mailbox.Token) == "" {
			return fmt.Errorf("%w: mailbox %q token is required", ErrInvalidRequest, name)
		}
		if !mailboxpkg.ValidForkMailboxToken(mailbox.Token) {
			return fmt.Errorf("%w: mailbox %q token is too long", ErrInvalidRequest, name)
		}
		if _, err := mailboxpkg.ForkMailboxMarker(name, mailbox.Token); err != nil {
			return fmt.Errorf("%w: mailbox %q marker is too long", ErrInvalidRequest, name)
		}
		if len(mailbox.Payload) == 0 {
			return fmt.Errorf("%w: mailbox %q payload is required", ErrInvalidRequest, name)
		}
		if len(mailbox.Payload) > mailboxpkg.ForkMailboxPayloadSize {
			return fmt.Errorf("%w: mailbox %q payload is too large", ErrInvalidRequest, name)
		}
		var payload map[string]any
		if err := json.Unmarshal(mailbox.Payload, &payload); err != nil || payload == nil {
			return fmt.Errorf("%w: mailbox %q payload must be a JSON object", ErrInvalidRequest, name)
		}
		if mailbox.WaitForAck {
			if mailbox.AckTimeout < 0 {
				return fmt.Errorf("%w: mailbox %q ack_timeout_ms must not be negative", ErrInvalidRequest, name)
			}
			if mailbox.AckTimeout > 30*time.Second {
				return fmt.Errorf("%w: mailbox %q ack_timeout_ms must be 30000 or less", ErrInvalidRequest, name)
			}
		}
	}
	return nil
}

func validateForkMailboxHypervisor(hvType hypervisor.Type) error {
	if hvType != hypervisor.TypeFirecracker {
		return fmt.Errorf("%w: mailboxes are only supported for %s standby forks", ErrNotSupported, hypervisor.TypeFirecracker)
	}
	return nil
}

func (m *manager) patchForkMailboxes(ctx context.Context, stored *StoredMetadata, snapshotDir string, mailboxes []ForkMailboxPayload) ([]patchedForkMailbox, error) {
	if len(mailboxes) == 0 {
		return nil, nil
	}

	patched := make([]patchedForkMailbox, 0, len(mailboxes))
	patches := make([]forkMailboxPatch, 0, len(mailboxes))
	for _, mailbox := range mailboxes {
		payload := mailbox.Payload
		var waiter *guestResumeNetworkUDPWaiter
		if mailbox.WaitForAck {
			var err error
			waiter, err = startGuestResumeNetworkUDPWaiter()
			if err != nil {
				closePatchedForkMailboxes(patched)
				return nil, fmt.Errorf("start mailbox %q UDP ack waiter: %w", mailbox.Name, err)
			}
			payload, err = forkMailboxPayloadWithAckPort(payload, waiter.Port())
			if err != nil {
				waiter.Close()
				closePatchedForkMailboxes(patched)
				return nil, err
			}
		}

		patches = append(patches, forkMailboxPatch{
			name:    mailbox.Name,
			token:   mailbox.Token,
			payload: payload,
		})
		patched = append(patched, patchedForkMailbox{
			name:    mailbox.Name,
			waiter:  waiter,
			timeout: forkMailboxAckTimeout(mailbox.AckTimeout),
		})
	}
	if err := m.patchForkMailboxPayloads(ctx, stored, snapshotDir, patches); err != nil {
		closePatchedForkMailboxes(patched)
		return nil, err
	}
	return patched, nil
}

func forkMailboxAckTimeout(timeout time.Duration) time.Duration {
	if timeout > 0 {
		return timeout
	}
	return 5 * time.Second
}

func closePatchedForkMailboxes(patched []patchedForkMailbox) {
	for _, mailbox := range patched {
		if mailbox.waiter != nil {
			mailbox.waiter.Close()
		}
	}
}

func (m *manager) waitForForkMailboxAcks(ctx context.Context, stored *StoredMetadata, patched []patchedForkMailbox) error {
	log := logger.FromContext(ctx)
	for _, mailbox := range patched {
		if mailbox.waiter == nil {
			continue
		}

		waitCtx, waitSpanEnd := m.startLifecycleStep(ctx, "guest.fork_mailbox.udp_ack_wait",
			attribute.String("instance_id", stored.Id),
			attribute.String("mailbox", mailbox.name),
			attribute.String("operation", "guest_fork_mailbox_udp_ack_wait"),
		)
		waitCtx, cancel := context.WithTimeout(waitCtx, mailbox.timeout)
		elapsed, ack, err := mailbox.waiter.WaitMailboxApplied(waitCtx, mailbox.name)
		cancel()
		waitSpanEnd(err)
		if err != nil {
			return fmt.Errorf("wait for mailbox %q ack: %w", mailbox.name, err)
		}
		log.InfoContext(ctx, "guest fork mailbox UDP ack received", "instance_id", stored.Id, "mailbox", mailbox.name, "elapsed", elapsed, "ack", ack)
	}
	return nil
}

func forkMailboxPayloadWithAckPort(payload json.RawMessage, port uint32) (json.RawMessage, error) {
	var obj map[string]any
	if err := json.Unmarshal(payload, &obj); err != nil {
		return nil, fmt.Errorf("unmarshal mailbox payload for ack injection: %w", err)
	}
	if obj == nil {
		return nil, fmt.Errorf("%w: mailbox payload must be a JSON object when wait_for_ack is true", ErrInvalidRequest)
	}
	obj["ack_port"] = port
	out, err := json.Marshal(obj)
	if err != nil {
		return nil, fmt.Errorf("marshal mailbox payload with ack port: %w", err)
	}
	if len(out) > mailboxpkg.ForkMailboxPayloadSize {
		return nil, fmt.Errorf("%w: mailbox payload is too large after ack_port injection", ErrInvalidRequest)
	}
	return out, nil
}

func (m *manager) patchForkMailboxPayloads(ctx context.Context, stored *StoredMetadata, snapshotDir string, patches []forkMailboxPatch) error {
	file, err := os.OpenFile(filepath.Join(snapshotDir, firecrackerSnapshotMemoryFile), os.O_RDWR, 0)
	if err != nil {
		return fmt.Errorf("open snapshot memory for fork mailboxes: %w", err)
	}
	defer file.Close()

	info, err := file.Stat()
	if err != nil {
		return fmt.Errorf("stat snapshot memory for fork mailboxes: %w", err)
	}
	if info.Size() <= 0 {
		return fmt.Errorf("fork mailbox memory file is empty")
	}

	type preparedForkMailboxPatch struct {
		patch  forkMailboxPatch
		offset int64
		finish func(error)
	}
	prepared := make([]preparedForkMailboxPatch, 0, len(patches))
	finishPrepared := func(err error) {
		for _, item := range prepared {
			item.finish(err)
		}
	}

	for _, patch := range patches {
		_, patchSpanEnd := m.startLifecycleStep(ctx, "guest.fork_mailbox.patch",
			attribute.String("instance_id", stored.Id),
			attribute.String("mailbox", patch.name),
			attribute.String("operation", "guest_fork_mailbox_patch"),
		)
		marker, err := mailboxpkg.ForkMailboxMarker(patch.name, patch.token)
		if err == nil {
			var offset int64
			offset, err = mailboxpkg.FindMarker(file, info.Size(), marker, &forkMailboxOffsetByMark)
			if err == nil {
				err = mailboxpkg.EnsurePayloadFits(mailboxpkg.ForkLayout, info.Size(), offset, len(patch.payload))
			}
			if err == nil {
				prepared = append(prepared, preparedForkMailboxPatch{
					patch:  patch,
					offset: offset,
					finish: patchSpanEnd,
				})
				continue
			}
		}
		patchSpanEnd(err)
		finishPrepared(err)
		return fmt.Errorf("preflight mailbox %q: %w", patch.name, err)
	}

	for i, item := range prepared {
		err := mailboxpkg.WritePayloadAt(file, mailboxpkg.ForkLayout, item.offset, item.patch.payload)
		item.finish(err)
		if err != nil {
			for _, pending := range prepared[i+1:] {
				pending.finish(err)
			}
			return fmt.Errorf("write mailbox %q frame: %w", item.patch.name, err)
		}
	}
	return nil
}
