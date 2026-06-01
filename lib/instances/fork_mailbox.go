package instances

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/kernel/hypeman/lib/logger"
	mailboxpkg "github.com/kernel/hypeman/lib/mailbox"
	"go.opentelemetry.io/otel/attribute"
	"golang.org/x/sys/unix"
)

var forkMailboxOffsetByMark sync.Map

type patchedForkMailbox struct {
	name    string
	waiter  *guestResumeNetworkUDPWaiter
	timeout time.Duration
}

func validateForkMailboxes(mailboxes []ForkMailboxPayload) error {
	if len(mailboxes) > 16 {
		return fmt.Errorf("%w: at most 16 mailboxes can be patched for a fork", ErrInvalidRequest)
	}
	seen := make(map[string]struct{}, len(mailboxes))
	for _, mailbox := range mailboxes {
		name := strings.TrimSpace(mailbox.Name)
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
		if mailbox.WaitForAck && mailbox.AckTimeout < 0 {
			return fmt.Errorf("%w: mailbox %q ack_timeout_ms must be positive", ErrInvalidRequest, name)
		}
		if mailbox.AckTimeout > 30*time.Second {
			return fmt.Errorf("%w: mailbox %q ack_timeout_ms must be 30000 or less", ErrInvalidRequest, name)
		}
	}
	return nil
}

func (m *manager) patchForkMailboxes(ctx context.Context, snapshotDir string, mailboxes []ForkMailboxPayload) ([]patchedForkMailbox, error) {
	if len(mailboxes) == 0 {
		return nil, nil
	}

	patched := make([]patchedForkMailbox, 0, len(mailboxes))
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

		if err := patchForkMailbox(snapshotDir, mailbox.Name, mailbox.Token, payload); err != nil {
			if waiter != nil {
				waiter.Close()
			}
			closePatchedForkMailboxes(patched)
			return nil, err
		}
		patched = append(patched, patchedForkMailbox{
			name:    mailbox.Name,
			waiter:  waiter,
			timeout: forkMailboxAckTimeout(mailbox.AckTimeout),
		})
	}
	return patched, nil
}

func forkMailboxAckTimeout(timeout time.Duration) time.Duration {
	if timeout > 0 {
		return timeout
	}
	return 2 * time.Second
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

func patchForkMailbox(snapshotDir, name, token string, payload []byte) error {
	if !mailboxpkg.ValidForkMailboxName(name) {
		return fmt.Errorf("invalid mailbox name %q", name)
	}
	if !mailboxpkg.ValidForkMailboxToken(token) {
		return fmt.Errorf("mailbox %q token is empty", name)
	}
	if len(payload) > mailboxpkg.ForkMailboxPayloadSize {
		return fmt.Errorf("mailbox %q payload too large: %d bytes", name, len(payload))
	}

	marker, err := mailboxpkg.ForkMailboxMarker(name, token)
	if err != nil {
		return fmt.Errorf("build mailbox %q marker: %w", name, err)
	}

	file, err := os.OpenFile(filepath.Join(snapshotDir, firecrackerSnapshotMemoryFile), os.O_RDWR, 0)
	if err != nil {
		return fmt.Errorf("open snapshot memory for mailbox %q: %w", name, err)
	}
	defer file.Close()

	info, err := file.Stat()
	if err != nil {
		return fmt.Errorf("stat snapshot memory for mailbox %q: %w", name, err)
	}
	if info.Size() <= 0 {
		return fmt.Errorf("mailbox %q memory file is empty", name)
	}

	idx, err := findForkMailbox(file, info.Size(), marker)
	if err != nil {
		return fmt.Errorf("find mailbox %q marker: %w", name, err)
	}
	if idx+int64(mailboxpkg.ForkMailboxPayloadOffset)+int64(len(payload)) > info.Size() {
		return fmt.Errorf("mailbox %q marker is too close to end of memory file", name)
	}

	if _, err := file.WriteAt(payload, idx+int64(mailboxpkg.ForkMailboxPayloadOffset)); err != nil {
		return fmt.Errorf("write mailbox %q payload: %w", name, err)
	}
	var u32 [4]byte
	binary.LittleEndian.PutUint32(u32[:], uint32(len(payload)))
	if _, err := file.WriteAt(u32[:], idx+int64(mailboxpkg.ForkMailboxLengthOffset)); err != nil {
		return fmt.Errorf("write mailbox %q payload length: %w", name, err)
	}
	binary.LittleEndian.PutUint32(u32[:], 1)
	if _, err := file.WriteAt(u32[:], idx+int64(mailboxpkg.ForkMailboxSeqOffset)); err != nil {
		return fmt.Errorf("write mailbox %q sequence: %w", name, err)
	}
	return nil
}

func findForkMailbox(file *os.File, size int64, marker []byte) (int64, error) {
	cacheKey := string(marker)
	if cached, ok := forkMailboxOffsetByMark.Load(cacheKey); ok {
		if offset, ok := cached.(int64); ok && offset >= 0 && offset+int64(len(marker)) <= size {
			buf := make([]byte, len(marker))
			if _, err := file.ReadAt(buf, offset); err == nil && bytes.Equal(buf, marker) {
				return offset, nil
			}
		}
	}

	data, err := unix.Mmap(int(file.Fd()), 0, int(size), unix.PROT_READ, unix.MAP_SHARED)
	if err != nil {
		return 0, fmt.Errorf("mmap snapshot memory: %w", err)
	}
	defer unix.Munmap(data)

	idx := bytes.Index(data, marker)
	if idx < 0 {
		return 0, fmt.Errorf("marker not found")
	}
	forkMailboxOffsetByMark.Store(cacheKey, int64(idx))
	return int64(idx), nil
}
