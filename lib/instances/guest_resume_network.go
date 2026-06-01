package instances

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	stdnet "net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/kernel/hypeman/lib/hypervisor"
	"github.com/kernel/hypeman/lib/logger"
	"go.opentelemetry.io/otel/attribute"
	"golang.org/x/sys/unix"
)

const guestResumeNetworkMailboxEnv = "HYPEMAN_RESUME_NETWORK_MAILBOX"
const guestResumeNetworkMailboxTokenEnv = "HYPEMAN_RESUME_NETWORK_MAILBOX_TOKEN"
const firecrackerSnapshotMemoryFile = "memory"

const guestResumeNetworkMailboxSeqOffset = 64
const guestResumeNetworkMailboxLengthOffset = 68
const guestResumeNetworkMailboxPayloadOffset = 72

var guestResumeNetworkMailboxMagic = []byte("HYPEMAN_RESUME_NETWORK_MAILBOX_V1\x00")
var guestResumeNetworkMailboxOffsets sync.Map

type guestResumeNetworkPayload struct {
	InterfaceName string `json:"interface_name"`
	MAC           string `json:"mac"`
	IPv4          string `json:"ipv4"`
	Prefix        uint32 `json:"prefix"`
	Gateway       string `json:"gateway"`
	AckPort       uint32 `json:"ack_port,omitempty"`
}

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
		strings.TrimSpace(stored.Env[guestResumeNetworkMailboxEnv]) == "1" &&
		token != "" &&
		len(token) <= guestResumeNetworkMailboxSeqOffset-len(guestResumeNetworkMailboxMagic)
}

func guestInitiatedResumeNetworkMailboxToken(stored *StoredMetadata) string {
	if stored == nil {
		return ""
	}
	return strings.TrimSpace(stored.Env[guestResumeNetworkMailboxTokenEnv])
}

func newGuestResumeNetworkPayload(cfg *guestNetworkConfig) guestResumeNetworkPayload {
	return guestResumeNetworkPayload{
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
	waitCtx, cancel := context.WithTimeout(waitCtx, 2*time.Second)
	defer cancel()

	elapsed, ack, err := waiter.WaitApplied(waitCtx, cfg.mac, cfg.ip)
	waitSpanEnd(err)
	if err != nil {
		return err
	}
	log.InfoContext(ctx, "guest resume network UDP ack received", "instance_id", stored.Id, "elapsed", elapsed, "ack", ack)
	return nil
}

func patchGuestResumeNetworkMailbox(snapshotDir, token string, payload *guestResumeNetworkPayload) error {
	if token == "" {
		return fmt.Errorf("resume network mailbox token is empty")
	}
	if len(token) > guestResumeNetworkMailboxSeqOffset-len(guestResumeNetworkMailboxMagic) {
		return fmt.Errorf("resume network mailbox token is too long")
	}
	if payload == nil {
		return fmt.Errorf("resume network mailbox payload is nil")
	}

	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal resume network mailbox payload: %w", err)
	}
	if len(payloadBytes) > 4096-guestResumeNetworkMailboxPayloadOffset {
		return fmt.Errorf("resume network mailbox payload too large: %d bytes", len(payloadBytes))
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

	marker := make([]byte, 0, len(guestResumeNetworkMailboxMagic)+len(token))
	marker = append(marker, guestResumeNetworkMailboxMagic...)
	marker = append(marker, []byte(token)...)

	idx, err := findGuestResumeNetworkMailbox(file, info.Size(), marker, token)
	if err != nil {
		return err
	}
	if idx+int64(guestResumeNetworkMailboxPayloadOffset)+int64(len(payloadBytes)) > info.Size() {
		return fmt.Errorf("resume network mailbox marker is too close to end of memory file")
	}

	if _, err := file.WriteAt(payloadBytes, idx+int64(guestResumeNetworkMailboxPayloadOffset)); err != nil {
		return fmt.Errorf("write resume network mailbox payload: %w", err)
	}
	var u32 [4]byte
	binary.LittleEndian.PutUint32(u32[:], uint32(len(payloadBytes)))
	if _, err := file.WriteAt(u32[:], idx+int64(guestResumeNetworkMailboxLengthOffset)); err != nil {
		return fmt.Errorf("write resume network mailbox payload length: %w", err)
	}
	binary.LittleEndian.PutUint32(u32[:], 1)
	if _, err := file.WriteAt(u32[:], idx+int64(guestResumeNetworkMailboxSeqOffset)); err != nil {
		return fmt.Errorf("write resume network mailbox sequence: %w", err)
	}
	return nil
}

func findGuestResumeNetworkMailbox(file *os.File, size int64, marker []byte, token string) (int64, error) {
	if cached, ok := guestResumeNetworkMailboxOffsets.Load(token); ok {
		if offset, ok := cached.(int64); ok && offset >= 0 && offset+int64(len(marker)) <= size {
			buf := make([]byte, len(marker))
			if _, err := file.ReadAt(buf, offset); err == nil && bytes.Equal(buf, marker) {
				return offset, nil
			}
		}
	}

	data, err := unix.Mmap(int(file.Fd()), 0, int(size), unix.PROT_READ, unix.MAP_SHARED)
	if err != nil {
		return 0, fmt.Errorf("mmap snapshot memory for resume network mailbox: %w", err)
	}
	defer unix.Munmap(data)

	idx := bytes.Index(data, marker)
	if idx < 0 {
		return 0, fmt.Errorf("resume network mailbox marker not found")
	}
	guestResumeNetworkMailboxOffsets.Store(token, int64(idx))
	return int64(idx), nil
}
