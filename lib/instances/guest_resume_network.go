package instances

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	stdnet "net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/kernel/hypeman/lib/hypervisor"
	"github.com/kernel/hypeman/lib/logger"
	"github.com/kernel/hypeman/lib/uffdpager"
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

type guestResumeNetworkUDPWaitResult struct {
	appliedElapsed time.Duration
	appliedAck     string
	stageElapsed   map[string]time.Duration
	stageAck       map[string]string
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

func (w *guestResumeNetworkUDPWaiter) WaitApplied(ctx context.Context, mac, ip string) (guestResumeNetworkUDPWaitResult, error) {
	if w == nil {
		return guestResumeNetworkUDPWaitResult{}, fmt.Errorf("guest resume network UDP waiter is nil")
	}

	start := time.Now()
	wantMAC := "mac=" + strings.ToLower(mac)
	wantIP := "ip=" + ip
	result := guestResumeNetworkUDPWaitResult{
		stageElapsed: make(map[string]time.Duration),
		stageAck:     make(map[string]string),
	}
	for {
		select {
		case ack := <-w.ch:
			text := strings.ToLower(ack.text)
			if !strings.Contains(text, wantMAC) || !strings.Contains(text, wantIP) {
				continue
			}
			if stage, ok := guestResumeNetworkAckStage(text); ok {
				if _, exists := result.stageElapsed[stage]; !exists {
					result.stageElapsed[stage] = ack.received.Sub(start)
					result.stageAck[stage] = ack.text
					if deepTrace := restoreDeepTraceFromContext(ctx); deepTrace != nil {
						deepTrace.Mark("guest_"+stage+"_received", ack.text)
						deepTrace.Sample("guest_" + stage + "_received")
					}
				}
			}
			if strings.Contains(text, "stage=applied") {
				result.appliedElapsed = ack.received.Sub(start)
				result.appliedAck = ack.text
				return result, nil
			}
		case <-ctx.Done():
			return guestResumeNetworkUDPWaitResult{}, ctx.Err()
		}
	}
}

func guestResumeNetworkAckStage(text string) (string, bool) {
	for _, field := range strings.Fields(text) {
		if stage, ok := strings.CutPrefix(field, "stage="); ok && stage != "" {
			return stage, true
		}
	}
	return "", false
}

func (m *manager) waitForGuestResumeNetworkUDPAck(ctx context.Context, waiter *guestResumeNetworkUDPWaiter, stored *StoredMetadata, cfg *guestNetworkConfig) error {
	if waiter == nil || cfg == nil {
		return nil
	}

	log := logger.FromContext(ctx)
	waitCtx, waitSpanEnd := m.startLifecycleStep(ctx, "guest.resume_network.fault_guest_memory_from_disk",
		attribute.String("instance_id", stored.Id),
		attribute.String("hypervisor", string(stored.HypervisorType)),
		attribute.String("operation", "guest_resume_network_fault_guest_memory_from_disk"),
		attribute.String("wait_for", "guest_network_applied_ack"),
		attribute.String("observed_dominant_wait", "fault_guest_memory_from_disk"),
	)
	waitCtx, cancel := context.WithTimeout(waitCtx, 2*time.Second)
	defer cancel()

	result, err := waiter.WaitApplied(waitCtx, cfg.mac, cfg.ip)
	waitSpanEnd(err)
	if err != nil {
		return err
	}
	log.InfoContext(ctx, "guest resume network UDP ack received", "instance_id", stored.Id, "elapsed", result.appliedElapsed, "ack", result.appliedAck, "stages", result.stageElapsed)
	return nil
}

func patchGuestResumeNetworkMailbox(snapshotDir, token string, payload *guestResumeNetworkPayload) error {
	payloadBytes, idx, file, err := prepareGuestResumeNetworkMailbox(snapshotDir, token, payload, os.O_RDWR)
	if err != nil {
		return err
	}
	defer file.Close()

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

func buildGuestResumeNetworkMailboxOverlay(snapshotDir, token string, payload *guestResumeNetworkPayload) (uffdpager.OverlayPage, error) {
	payloadBytes, idx, file, err := prepareGuestResumeNetworkMailbox(snapshotDir, token, payload, os.O_RDONLY)
	if err != nil {
		return uffdpager.OverlayPage{}, err
	}
	defer file.Close()

	const pageSize = 4096
	pageOffset := (idx / pageSize) * pageSize
	pageRelative := idx - pageOffset
	if pageRelative+int64(guestResumeNetworkMailboxPayloadOffset)+int64(len(payloadBytes)) > pageSize {
		return uffdpager.OverlayPage{}, fmt.Errorf("resume network mailbox crosses a page boundary")
	}

	page := make([]byte, pageSize)
	n, err := file.ReadAt(page, pageOffset)
	if err != nil && !errors.Is(err, io.EOF) {
		return uffdpager.OverlayPage{}, fmt.Errorf("read resume network mailbox overlay source page: %w", err)
	}
	if n == 0 && err != nil {
		return uffdpager.OverlayPage{}, fmt.Errorf("read resume network mailbox overlay source page: %w", err)
	}

	base := int(pageRelative)
	copy(page[base+guestResumeNetworkMailboxPayloadOffset:], payloadBytes)
	binary.LittleEndian.PutUint32(page[base+guestResumeNetworkMailboxLengthOffset:], uint32(len(payloadBytes)))
	binary.LittleEndian.PutUint32(page[base+guestResumeNetworkMailboxSeqOffset:], 1)

	overlayDir := filepath.Join(snapshotDir, "uffd-overlays")
	if err := os.MkdirAll(overlayDir, 0755); err != nil {
		return uffdpager.OverlayPage{}, fmt.Errorf("create uffd overlay directory: %w", err)
	}
	overlayPath := filepath.Join(overlayDir, fmt.Sprintf("resume-network-mailbox-%d.page", pageOffset))
	if err := os.WriteFile(overlayPath, page, 0644); err != nil {
		return uffdpager.OverlayPage{}, fmt.Errorf("write resume network mailbox overlay page: %w", err)
	}
	return uffdpager.OverlayPage{GuestMemoryOffset: pageOffset, Path: overlayPath}, nil
}

func prepareGuestResumeNetworkMailbox(snapshotDir, token string, payload *guestResumeNetworkPayload, flag int) ([]byte, int64, *os.File, error) {
	if token == "" {
		return nil, 0, nil, fmt.Errorf("resume network mailbox token is empty")
	}
	if len(token) > guestResumeNetworkMailboxSeqOffset-len(guestResumeNetworkMailboxMagic) {
		return nil, 0, nil, fmt.Errorf("resume network mailbox token is too long")
	}
	if payload == nil {
		return nil, 0, nil, fmt.Errorf("resume network mailbox payload is nil")
	}

	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		return nil, 0, nil, fmt.Errorf("marshal resume network mailbox payload: %w", err)
	}
	if len(payloadBytes) > 4096-guestResumeNetworkMailboxPayloadOffset {
		return nil, 0, nil, fmt.Errorf("resume network mailbox payload too large: %d bytes", len(payloadBytes))
	}

	file, err := os.OpenFile(filepath.Join(snapshotDir, firecrackerSnapshotMemoryFile), flag, 0)
	if err != nil {
		return nil, 0, nil, fmt.Errorf("open snapshot memory for resume network mailbox: %w", err)
	}

	info, err := file.Stat()
	if err != nil {
		file.Close()
		return nil, 0, nil, fmt.Errorf("stat snapshot memory for resume network mailbox: %w", err)
	}
	if info.Size() <= 0 {
		file.Close()
		return nil, 0, nil, fmt.Errorf("resume network mailbox memory file is empty")
	}

	marker := make([]byte, 0, len(guestResumeNetworkMailboxMagic)+len(token))
	marker = append(marker, guestResumeNetworkMailboxMagic...)
	marker = append(marker, []byte(token)...)

	idx, err := findGuestResumeNetworkMailbox(file, info.Size(), marker, token)
	if err != nil {
		file.Close()
		return nil, 0, nil, err
	}
	if idx+int64(guestResumeNetworkMailboxPayloadOffset)+int64(len(payloadBytes)) > info.Size() {
		file.Close()
		return nil, 0, nil, fmt.Errorf("resume network mailbox marker is too close to end of memory file")
	}
	return payloadBytes, idx, file, nil
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
