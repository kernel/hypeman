package instances

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	stdnet "net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/kernel/hypeman/lib/guest"
	"github.com/kernel/hypeman/lib/hypervisor"
	"github.com/kernel/hypeman/lib/logger"
	"go.opentelemetry.io/otel/attribute"
	"golang.org/x/sys/unix"
)

const guestResumeNetworkPortEnv = "HYPEMAN_RESUME_NETWORK_PORT"
const guestResumeNetworkPrearmEnv = "HYPEMAN_RESUME_NETWORK_PREARM"
const guestResumeNetworkStartArmedEnv = "HYPEMAN_RESUME_NETWORK_START_ARMED"
const guestResumeNetworkArmedPollIntervalEnv = "HYPEMAN_RESUME_NETWORK_ARMED_POLL_INTERVAL_MS"
const guestResumeNetworkPrearmSettleEnv = "HYPEMAN_RESUME_NETWORK_PREARM_SETTLE_MS"
const guestResumeNetworkMailboxEnv = "HYPEMAN_RESUME_NETWORK_MAILBOX"
const guestResumeNetworkMailboxTokenEnv = "HYPEMAN_RESUME_NETWORK_MAILBOX_TOKEN"
const guestResumeNetworkAckPortEnv = "HYPEMAN_RESUME_NETWORK_ACK_PORT"
const firecrackerRestoreMetadataFile = "firecracker-config.json"
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

type guestResumeNetworkResult struct {
	ack     string
	elapsed time.Duration
	err     error
}

type guestResumeNetworkServer struct {
	path    string
	payload *guestResumeNetworkPayload

	listener stdnet.Listener
	done     chan guestResumeNetworkResult
	armed    chan struct{}
	close    chan struct{}
	once     sync.Once
}

func guestInitiatedResumeNetworkPort(stored *StoredMetadata) int {
	if stored == nil || stored.HypervisorType != hypervisor.TypeFirecracker {
		return 0
	}
	rawPort := strings.TrimSpace(stored.Env[guestResumeNetworkPortEnv])
	if rawPort == "" {
		return 0
	}
	port, err := strconv.Atoi(rawPort)
	if err != nil || port <= 0 {
		return 0
	}
	return port
}

func guestInitiatedResumeNetworkPrearm(stored *StoredMetadata) bool {
	return guestInitiatedResumeNetworkPort(stored) != 0 && strings.TrimSpace(stored.Env[guestResumeNetworkPrearmEnv]) == "1"
}

func guestInitiatedResumeNetworkMailbox(stored *StoredMetadata) bool {
	return guestInitiatedResumeNetworkPrearm(stored) && strings.TrimSpace(stored.Env[guestResumeNetworkMailboxEnv]) == "1"
}

func guestInitiatedResumeNetworkMailboxToken(stored *StoredMetadata) string {
	if stored == nil {
		return ""
	}
	return strings.TrimSpace(stored.Env[guestResumeNetworkMailboxTokenEnv])
}

func guestInitiatedResumeNetworkAckPort(stored *StoredMetadata) uint32 {
	if stored == nil {
		return 0
	}
	raw := strings.TrimSpace(stored.Env[guestResumeNetworkAckPortEnv])
	if raw == "" {
		return 0
	}
	port, err := strconv.Atoi(raw)
	if err != nil || port <= 0 || port > 65535 {
		return 0
	}
	return uint32(port)
}

func guestInitiatedResumeNetworkStartArmed(stored *StoredMetadata) bool {
	return guestInitiatedResumeNetworkPrearm(stored) && strings.TrimSpace(stored.Env[guestResumeNetworkStartArmedEnv]) == "1"
}

func guestInitiatedResumeNetworkArmedPollIntervalMS(stored *StoredMetadata) uint32 {
	if stored == nil {
		return 0
	}
	raw := strings.TrimSpace(stored.Env[guestResumeNetworkArmedPollIntervalEnv])
	if raw == "" {
		return 0
	}
	ms, err := strconv.Atoi(raw)
	if err != nil || ms <= 0 {
		return 0
	}
	return uint32(ms)
}

func guestInitiatedResumeNetworkPrearmSettle(stored *StoredMetadata) time.Duration {
	if stored == nil {
		return 0
	}
	raw := strings.TrimSpace(stored.Env[guestResumeNetworkPrearmSettleEnv])
	if raw == "" {
		return 0
	}
	ms, err := strconv.Atoi(raw)
	if err != nil || ms <= 0 {
		return 0
	}
	return time.Duration(ms) * time.Millisecond
}

func guestInitiatedResumeNetworkSocket(stored *StoredMetadata) string {
	if stored == nil || stored.HypervisorType != hypervisor.TypeFirecracker {
		return ""
	}

	sourceDataDir := firecrackerSnapshotSourceDataDir(stored.DataDir)
	if sourceDataDir == "" || filepath.Clean(sourceDataDir) == filepath.Clean(stored.DataDir) {
		return stored.VsockSocket
	}
	return filepath.Join(sourceDataDir, filepath.Base(stored.VsockSocket))
}

func firecrackerSnapshotSourceDataDir(dataDir string) string {
	if dataDir == "" {
		return ""
	}

	data, err := os.ReadFile(filepath.Join(dataDir, firecrackerRestoreMetadataFile))
	if err != nil {
		return ""
	}
	var meta struct {
		SnapshotSourceDataDir string `json:"snapshot_source_data_dir"`
	}
	if err := json.Unmarshal(data, &meta); err != nil {
		return ""
	}
	return strings.TrimSpace(meta.SnapshotSourceDataDir)
}

func startGuestResumeNetworkServer(ctx context.Context, vsockSocket string, port int, payload *guestResumeNetworkPayload) (*guestResumeNetworkServer, error) {
	path := fmt.Sprintf("%s_%d", vsockSocket, port)
	_ = os.Remove(path)

	listener, err := stdnet.Listen("unix", path)
	if err != nil {
		return nil, fmt.Errorf("listen on guest-initiated vsock socket %s: %w", path, err)
	}

	s := &guestResumeNetworkServer{
		path:     path,
		payload:  payload,
		listener: listener,
		done:     make(chan guestResumeNetworkResult, 1),
		armed:    make(chan struct{}, 1),
		close:    make(chan struct{}),
	}

	go func() {
		<-ctx.Done()
		s.Close()
	}()
	go s.acceptLoop()

	return s, nil
}

func (s *guestResumeNetworkServer) Close() {
	s.once.Do(func() {
		close(s.close)
		_ = s.listener.Close()
		_ = os.Remove(s.path)
	})
}

func (s *guestResumeNetworkServer) WaitArmed(ctx context.Context) error {
	select {
	case <-s.armed:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *guestResumeNetworkServer) WaitApplied(ctx context.Context) (time.Duration, string, error) {
	select {
	case result := <-s.done:
		return result.elapsed, result.ack, result.err
	case <-ctx.Done():
		return 0, "", ctx.Err()
	}
}

func (s *guestResumeNetworkServer) acceptLoop() {
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			select {
			case <-s.close:
				return
			default:
				s.reportDone(guestResumeNetworkResult{err: err})
				return
			}
		}
		go s.handleConn(conn)
	}
}

func (s *guestResumeNetworkServer) handleConn(conn stdnet.Conn) {
	defer conn.Close()

	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	reader := bufio.NewReader(conn)
	line, err := reader.ReadString('\n')
	if err != nil {
		s.reportDone(guestResumeNetworkResult{err: fmt.Errorf("read resume network command: %w", err)})
		return
	}

	switch strings.TrimSpace(line) {
	case "HELLO":
		slog.Info("guest resume network control connection armed", "socket", s.path)
		select {
		case s.armed <- struct{}{}:
		default:
		}
		_ = conn.SetDeadline(time.Time{})
		_, _ = io.Copy(io.Discard, reader)
	case "FETCH":
		slog.Info("guest resume network config fetch received", "socket", s.path)
		s.handleFetch(conn, reader)
	default:
		s.reportDone(guestResumeNetworkResult{err: fmt.Errorf("unexpected resume network command %q", strings.TrimSpace(line))})
	}
}

func (s *guestResumeNetworkServer) handleFetch(conn stdnet.Conn, reader *bufio.Reader) {
	start := time.Now()
	if s.payload == nil {
		s.reportDone(guestResumeNetworkResult{err: fmt.Errorf("resume network fetch received without payload")})
		return
	}

	if err := json.NewEncoder(conn).Encode(s.payload); err != nil {
		s.reportDone(guestResumeNetworkResult{err: fmt.Errorf("write resume network payload: %w", err)})
		return
	}

	ack, err := reader.ReadString('\n')
	elapsed := time.Since(start)
	if err != nil {
		s.reportDone(guestResumeNetworkResult{elapsed: elapsed, err: fmt.Errorf("read resume network ack: %w", err)})
		return
	}
	ack = strings.TrimSpace(ack)
	if !strings.HasPrefix(ack, "OK") {
		s.reportDone(guestResumeNetworkResult{elapsed: elapsed, ack: ack, err: fmt.Errorf("resume network failed: %s", ack)})
		return
	}
	s.reportDone(guestResumeNetworkResult{elapsed: elapsed, ack: ack})
}

func (s *guestResumeNetworkServer) reportDone(result guestResumeNetworkResult) {
	select {
	case s.done <- result:
		go s.Close()
	default:
	}
}

func newGuestResumeNetworkPayload(stored *StoredMetadata, allocConfig *guestNetworkConfig) guestResumeNetworkPayload {
	return guestResumeNetworkPayload{
		InterfaceName: "eth0",
		MAC:           allocConfig.mac,
		IPv4:          allocConfig.ip,
		Prefix:        uint32(allocConfig.prefix),
		Gateway:       allocConfig.gateway,
		AckPort:       guestInitiatedResumeNetworkAckPort(stored),
	}
}

func patchGuestResumeNetworkMailbox(snapshotDir, token string, payload *guestResumeNetworkPayload) error {
	if token == "" {
		return fmt.Errorf("resume network mailbox token is empty")
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

	memoryPath := filepath.Join(snapshotDir, firecrackerSnapshotMemoryFile)
	file, err := os.OpenFile(memoryPath, os.O_RDWR, 0)
	if err != nil {
		return fmt.Errorf("open snapshot memory for resume network mailbox: %w", err)
	}
	defer file.Close()

	info, err := file.Stat()
	if err != nil {
		return fmt.Errorf("stat snapshot memory for resume network mailbox: %w", err)
	}
	if info.Size() <= 0 {
		return fmt.Errorf("snapshot memory file is empty")
	}
	if info.Size() > int64(int(info.Size())) {
		return fmt.Errorf("snapshot memory file too large to map: %d bytes", info.Size())
	}

	marker := make([]byte, 0, len(guestResumeNetworkMailboxMagic)+len(token))
	marker = append(marker, guestResumeNetworkMailboxMagic...)
	marker = append(marker, token...)

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
		idx := cached.(int64)
		if idx >= 0 && idx+int64(len(marker)) <= size {
			buf := make([]byte, len(marker))
			if _, err := file.ReadAt(buf, idx); err == nil && bytes.Equal(buf, marker) {
				return idx, nil
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

func (m *manager) startGuestInitiatedResumeNetwork(ctx context.Context, stored *StoredMetadata, vsockSocket string, allocConfig *guestNetworkConfig) (*guestResumeNetworkServer, context.CancelFunc, error) {
	port := guestInitiatedResumeNetworkPort(stored)
	if port == 0 {
		return nil, nil, nil
	}
	if vsockSocket == "" {
		vsockSocket = stored.VsockSocket
	}

	payload := newGuestResumeNetworkPayload(stored, allocConfig)
	serverCtx, cancel := context.WithCancel(ctx)
	server, err := startGuestResumeNetworkServer(serverCtx, vsockSocket, port, &payload)
	if err != nil {
		cancel()
		return nil, nil, err
	}
	return server, cancel, nil
}

func (m *manager) armGuestInitiatedResumeNetwork(ctx context.Context, stored *StoredMetadata) error {
	if !guestInitiatedResumeNetworkPrearm(stored) {
		return nil
	}

	dialer, err := hypervisor.NewVsockDialer(stored.HypervisorType, stored.VsockSocket, stored.VsockCID)
	if err != nil {
		return fmt.Errorf("create vsock dialer: %w", err)
	}

	return guest.ArmResumeNetworkInInstance(ctx, dialer, guest.ArmResumeNetworkOptions{
		PollIntervalMS: guestInitiatedResumeNetworkArmedPollIntervalMS(stored),
		WaitForAgent:   120 * time.Second,
	})
}

func (m *manager) waitForGuestInitiatedResumeNetwork(ctx context.Context, server *guestResumeNetworkServer, stored *StoredMetadata) error {
	log := logger.FromContext(ctx)
	waitCtx, waitSpanEnd := m.startLifecycleStep(ctx, "guest.resume_network.wait",
		attribute.String("instance_id", stored.Id),
		attribute.String("hypervisor", string(stored.HypervisorType)),
		attribute.String("operation", "guest_resume_network_wait"),
	)
	waitCtx, cancel := context.WithTimeout(waitCtx, 2*time.Second)
	defer cancel()

	elapsed, ack, err := server.WaitApplied(waitCtx)
	waitSpanEnd(err)
	if err != nil {
		return err
	}
	log.InfoContext(ctx, "guest-initiated resume network applied", "instance_id", stored.Id, "elapsed", elapsed, "ack", ack)
	return nil
}
