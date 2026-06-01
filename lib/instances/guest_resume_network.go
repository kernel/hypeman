package instances

import (
	"context"
	"fmt"
	stdnet "net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/kernel/hypeman/lib/hypervisor"
	"github.com/kernel/hypeman/lib/logger"
	"github.com/kernel/hypeman/lib/mailbox"
	"github.com/nrednav/cuid2"
	"go.opentelemetry.io/otel/attribute"
)

const firecrackerSnapshotMemoryFile = "memory"

const guestResumeNetworkUDPAckTimeout = 5 * time.Second

var guestResumeNetworkMailboxOffsets sync.Map

type guestResumeNetworkUDPAck struct {
	received time.Time
	text     string
}

type guestResumeNetworkUDPWaiter struct {
	conn *stdnet.UDPConn
	ch   chan guestResumeNetworkUDPAck
}

func guestInitiatedResumeNetworkMailbox(stored *StoredMetadata) bool {
	token := guestInitiatedResumeNetworkMailboxToken(stored)
	return stored != nil &&
		stored.HypervisorType == hypervisor.TypeFirecracker &&
		strings.TrimSpace(stored.Env[mailbox.MailboxEnv]) == "1" &&
		mailbox.ValidToken(token)
}

func guestInitiatedResumeNetworkMailboxToken(stored *StoredMetadata) string {
	if stored == nil {
		return ""
	}
	return strings.TrimSpace(stored.Env[mailbox.MailboxTokenEnv])
}

func ensureGuestInitiatedResumeNetworkMailbox(stored *StoredMetadata) {
	if stored == nil ||
		stored.HypervisorType != hypervisor.TypeFirecracker ||
		!stored.NetworkEnabled ||
		stored.SkipGuestAgent {
		return
	}
	if stored.Env == nil {
		stored.Env = make(map[string]string)
	}
	stored.Env[mailbox.MailboxEnv] = "1"
	if token := guestInitiatedResumeNetworkMailboxToken(stored); !mailbox.ValidToken(token) {
		stored.Env[mailbox.MailboxTokenEnv] = cuid2.Generate()
	}
}

func newGuestResumeNetworkPayload(cfg *guestNetworkConfig) mailbox.Payload {
	return mailbox.Payload{
		InterfaceName: "eth0",
		MAC:           cfg.mac,
		IPv4:          cfg.ip,
		Prefix:        uint32(cfg.prefix),
		Gateway:       cfg.gateway,
	}
}

func startGuestResumeNetworkUDPWaiter() (*guestResumeNetworkUDPWaiter, error) {
	conn, err := stdnet.ListenUDP("udp4", &stdnet.UDPAddr{IP: stdnet.IPv4zero, Port: 0})
	if err != nil {
		return nil, fmt.Errorf("listen for guest resume network UDP ack: %w", err)
	}

	w := &guestResumeNetworkUDPWaiter{
		conn: conn,
		ch:   make(chan guestResumeNetworkUDPAck, 128),
	}
	go w.readLoop()
	return w, nil
}

func (w *guestResumeNetworkUDPWaiter) Port() uint32 {
	if w == nil || w.conn == nil {
		return 0
	}
	return uint32(w.conn.LocalAddr().(*stdnet.UDPAddr).Port)
}

func (w *guestResumeNetworkUDPWaiter) Close() {
	if w == nil || w.conn == nil {
		return
	}
	_ = w.conn.Close()
}

func (w *guestResumeNetworkUDPWaiter) readLoop() {
	buf := make([]byte, 1024)
	for {
		n, _, err := w.conn.ReadFromUDP(buf)
		if err != nil {
			return
		}
		w.ch <- guestResumeNetworkUDPAck{
			received: time.Now(),
			text:     strings.TrimSpace(string(buf[:n])),
		}
	}
}

func (w *guestResumeNetworkUDPWaiter) WaitApplied(ctx context.Context, mac, ip string) (time.Duration, string, error) {
	if w == nil {
		return 0, "", fmt.Errorf("guest resume network UDP waiter is nil")
	}

	start := time.Now()
	wantMAC := "mac=" + strings.ToLower(mac)
	wantIP := "ip=" + ip
	for {
		select {
		case ack := <-w.ch:
			text := strings.ToLower(ack.text)
			if strings.Contains(text, "stage=applied") && strings.Contains(text, wantMAC) && strings.Contains(text, wantIP) {
				return ack.received.Sub(start), ack.text, nil
			}
		case <-ctx.Done():
			return 0, "", ctx.Err()
		}
	}
}

func (w *guestResumeNetworkUDPWaiter) WaitMailboxApplied(ctx context.Context, name string) (time.Duration, string, error) {
	if w == nil {
		return 0, "", fmt.Errorf("guest fork mailbox UDP waiter is nil")
	}

	start := time.Now()
	wantMailbox := "mailbox=" + strings.ToLower(name)
	for {
		select {
		case ack := <-w.ch:
			text := strings.ToLower(ack.text)
			if strings.Contains(text, "stage=applied") && strings.Contains(text, wantMailbox) {
				return ack.received.Sub(start), ack.text, nil
			}
		case <-ctx.Done():
			return 0, "", ctx.Err()
		}
	}
}

func (m *manager) waitForGuestResumeNetworkUDPAck(ctx context.Context, waiter *guestResumeNetworkUDPWaiter, stored *StoredMetadata, cfg *guestNetworkConfig) error {
	if waiter == nil || cfg == nil {
		return nil
	}

	log := logger.FromContext(ctx)
	waitCtx, waitSpanEnd := m.startLifecycleStep(ctx, "guest.resume_network.udp_ack_wait",
		attribute.String("instance_id", stored.Id),
		attribute.String("hypervisor", string(stored.HypervisorType)),
		attribute.String("operation", "guest_resume_network_udp_ack_wait"),
	)
	waitCtx, cancel := context.WithTimeout(waitCtx, guestResumeNetworkUDPAckTimeout)
	defer cancel()

	elapsed, ack, err := waiter.WaitApplied(waitCtx, cfg.mac, cfg.ip)
	waitSpanEnd(err)
	if err != nil {
		return err
	}
	log.InfoContext(ctx, "guest resume network UDP ack received", "instance_id", stored.Id, "elapsed", elapsed, "ack", ack)
	return nil
}

func patchGuestResumeNetworkMailbox(snapshotDir, token string, payload *mailbox.Payload) error {
	payloadBytes, err := mailbox.MarshalPayload(payload)
	if err != nil {
		return err
	}

	file, err := os.OpenFile(filepath.Join(snapshotDir, firecrackerSnapshotMemoryFile), os.O_RDWR, 0)
	if err != nil {
		return fmt.Errorf("open snapshot memory for resume network mailbox: %w", err)
	}
	defer file.Close()

	info, err := file.Stat()
	if err != nil {
		return fmt.Errorf("stat snapshot memory for resume network mailbox: %w", err)
	}
	if info.Size() <= 0 {
		return fmt.Errorf("resume network mailbox memory file is empty")
	}

	marker, err := mailbox.Marker(token)
	if err != nil {
		return err
	}
	idx, err := findGuestResumeNetworkMailbox(file, info.Size(), marker, token)
	if err != nil {
		return err
	}
	if err := mailbox.EnsurePayloadFits(mailbox.ResumeNetworkLayout, info.Size(), idx, len(payloadBytes)); err != nil {
		return fmt.Errorf("resume network mailbox payload does not fit: %w", err)
	}
	if err := mailbox.WritePayloadAt(file, mailbox.ResumeNetworkLayout, idx, payloadBytes); err != nil {
		return fmt.Errorf("write resume network mailbox frame: %w", err)
	}
	return nil
}

func findGuestResumeNetworkMailbox(file *os.File, size int64, marker []byte, token string) (int64, error) {
	idx, err := mailbox.FindMarker(file, size, marker, &guestResumeNetworkMailboxOffsets)
	if err != nil {
		return 0, fmt.Errorf("find resume network mailbox marker for token %q: %w", token, err)
	}
	return idx, nil
}
