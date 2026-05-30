//go:build linux

package main

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
	"unsafe"

	pb "github.com/kernel/hypeman/lib/guest"
	"github.com/mdlayher/socket"
	"golang.org/x/sys/unix"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const resumeNetworkPortEnv = "HYPEMAN_RESUME_NETWORK_PORT"
const resumeNetworkPollIntervalEnv = "HYPEMAN_RESUME_NETWORK_POLL_INTERVAL_MS"
const resumeNetworkTriggerEnv = "HYPEMAN_RESUME_NETWORK_TRIGGER"
const resumeNetworkPrearmEnv = "HYPEMAN_RESUME_NETWORK_PREARM"
const resumeNetworkStartArmedEnv = "HYPEMAN_RESUME_NETWORK_START_ARMED"
const resumeNetworkSlowPollIntervalEnv = "HYPEMAN_RESUME_NETWORK_SLOW_POLL_INTERVAL_MS"
const resumeNetworkArmedPollIntervalEnv = "HYPEMAN_RESUME_NETWORK_ARMED_POLL_INTERVAL_MS"
const resumeNetworkMailboxEnv = "HYPEMAN_RESUME_NETWORK_MAILBOX"
const resumeNetworkMailboxTokenEnv = "HYPEMAN_RESUME_NETWORK_MAILBOX_TOKEN"
const resumeNetworkTriggerVMGenID = "vmgenid"
const resumeNetworkHostCID = 2
const vmgenIDKmsgSignal = "crng reseeded due to virtual machine fork"
const defaultResumeNetworkSlowPollInterval = time.Second
const defaultResumeNetworkArmedPollInterval = time.Millisecond
const resumeNetworkMailboxSize = 4096
const resumeNetworkMailboxSeqOffset = 64
const resumeNetworkMailboxLengthOffset = 68
const resumeNetworkMailboxPayloadOffset = 72

var resumeNetworkMailboxMagic = []byte("HYPEMAN_RESUME_NETWORK_MAILBOX_V1\x00")

type resumeNetworkPayload struct {
	InterfaceName string `json:"interface_name"`
	MAC           string `json:"mac"`
	IPv4          string `json:"ipv4"`
	Prefix        uint32 `json:"prefix"`
	Gateway       string `json:"gateway"`
	AckPort       uint32 `json:"ack_port,omitempty"`
}

type resumeNetworkArmRequest struct {
	interval time.Duration
	ready    chan error
}

type vmGenIDResumeWaiter struct {
	file   *os.File
	reader *bufio.Reader
}

type resumeNetworkController struct {
	s            *guestServer
	port         uint32
	trigger      string
	startArmed   bool
	mailbox      []byte
	slowInterval time.Duration
	fastInterval time.Duration
	arm          chan resumeNetworkArmRequest
}

func startResumeNetworkWatcher(s *guestServer) {
	rawPort := strings.TrimSpace(os.Getenv(resumeNetworkPortEnv))
	if rawPort == "" {
		return
	}

	port, err := strconv.ParseUint(rawPort, 10, 32)
	if err != nil || port == 0 {
		log.Printf("[guest-agent] ignoring invalid %s=%q", resumeNetworkPortEnv, rawPort)
		return
	}

	if resumeNetworkPrearmEnabled() {
		controller := newResumeNetworkController(s, uint32(port))
		s.resumeNetwork = controller
		go controller.run()
		return
	}

	if resumeNetworkTrigger() == resumeNetworkTriggerVMGenID {
		go resumeNetworkVMGenIDLoop(s, uint32(port))
		return
	}

	if interval, ok := resumeNetworkPollInterval(); ok {
		go resumeNetworkPollLoop(s, uint32(port), interval)
		return
	}

	go resumeNetworkLoop(s, uint32(port))
}

func resumeNetworkPrearmEnabled() bool {
	return strings.TrimSpace(os.Getenv(resumeNetworkPrearmEnv)) == "1"
}

func newResumeNetworkController(s *guestServer, port uint32) *resumeNetworkController {
	mailbox := []byte(nil)
	if strings.TrimSpace(os.Getenv(resumeNetworkMailboxEnv)) == "1" {
		mailbox = newResumeNetworkMailbox()
	}
	return &resumeNetworkController{
		s:            s,
		port:         port,
		trigger:      resumeNetworkTrigger(),
		startArmed:   strings.TrimSpace(os.Getenv(resumeNetworkStartArmedEnv)) == "1",
		mailbox:      mailbox,
		slowInterval: resumeNetworkIntervalFromEnv(resumeNetworkSlowPollIntervalEnv, defaultResumeNetworkSlowPollInterval),
		fastInterval: resumeNetworkIntervalFromEnv(resumeNetworkArmedPollIntervalEnv, defaultResumeNetworkArmedPollInterval),
		arm:          make(chan resumeNetworkArmRequest),
	}
}

func newResumeNetworkMailbox() []byte {
	token := strings.TrimSpace(os.Getenv(resumeNetworkMailboxTokenEnv))
	if token == "" {
		log.Printf("[guest-agent] resume network mailbox disabled: missing %s", resumeNetworkMailboxTokenEnv)
		return nil
	}

	buf := make([]byte, resumeNetworkMailboxSize)
	copy(buf, resumeNetworkMailboxMagic)
	copy(buf[len(resumeNetworkMailboxMagic):resumeNetworkMailboxSeqOffset], token)
	if err := unix.Mlock(buf); err != nil {
		log.Printf("[guest-agent] resume network mailbox mlock failed: %v", err)
	}
	log.Printf("[guest-agent] resume network mailbox armed token=%s", token)
	return buf
}

func resumeNetworkIntervalFromEnv(name string, fallback time.Duration) time.Duration {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback
	}
	ms, err := strconv.Atoi(raw)
	if err != nil || ms <= 0 {
		log.Printf("[guest-agent] ignoring invalid %s=%q", name, raw)
		return fallback
	}
	return time.Duration(ms) * time.Millisecond
}

func resumeNetworkTrigger() string {
	return strings.ToLower(strings.TrimSpace(os.Getenv(resumeNetworkTriggerEnv)))
}

func resumeNetworkLoop(s *guestServer, port uint32) {
	for {
		if err := armResumeNetworkConnection(port); err != nil {
			log.Printf("[guest-agent] resume network arm failed: %v", err)
			time.Sleep(100 * time.Millisecond)
			continue
		}

		start := time.Now()
		if err := fetchAndApplyResumeNetwork(s, port, 50*time.Millisecond); err != nil {
			log.Printf("[guest-agent] resume network apply failed after wake: %v", err)
			time.Sleep(25 * time.Millisecond)
			continue
		}
		log.Printf("[guest-agent] resume network applied in %s", time.Since(start))
	}
}

func resumeNetworkPollInterval() (time.Duration, bool) {
	raw := strings.TrimSpace(os.Getenv(resumeNetworkPollIntervalEnv))
	if raw == "" {
		return 0, false
	}
	ms, err := strconv.Atoi(raw)
	if err != nil || ms <= 0 {
		log.Printf("[guest-agent] ignoring invalid %s=%q", resumeNetworkPollIntervalEnv, raw)
		return 0, false
	}
	return time.Duration(ms) * time.Millisecond, true
}

func resumeNetworkPollLoop(s *guestServer, port uint32, interval time.Duration) {
	log.Printf("[guest-agent] resume network polling host port %d every %s", port, interval)
	dialTimeout := resumeNetworkDialTimeout(interval)
	for {
		start := time.Now()
		if err := fetchAndApplyResumeNetwork(s, port, dialTimeout); err == nil {
			log.Printf("[guest-agent] resume network poll applied in %s", time.Since(start))
			time.Sleep(100 * time.Millisecond)
			continue
		}
		if remaining := interval - time.Since(start); remaining > 0 {
			time.Sleep(remaining)
		}
	}
}

func (c *resumeNetworkController) run() {
	log.Printf("[guest-agent] resume network prearm loop host port %d trigger=%s slow=%s fast=%s start_armed=%t", c.port, c.trigger, c.slowInterval, c.fastInterval, c.startArmed)
	armed := c.startArmed
	fastInterval := c.fastInterval

	applyArm := func(req resumeNetworkArmRequest) {
		if req.interval > 0 {
			fastInterval = req.interval
		}
		armed = true
	}

	for {
		var armReady chan error
		if !armed {
			select {
			case req := <-c.arm:
				applyArm(req)
				armReady = req.ready
			}
		}

		if c.mailbox != nil {
			var vmgenIDWaiter *vmGenIDResumeWaiter
			if c.trigger == resumeNetworkTriggerVMGenID {
				var err error
				vmgenIDWaiter, err = newVMGenIDResumeWaiter()
				if armReady != nil {
					armReady <- err
				}
				if err != nil {
					log.Printf("[guest-agent] resume network VMGenID prepare failed: %v", err)
					armed = false
					continue
				}
			} else if armReady != nil {
				armReady <- nil
			}

			start := time.Now()
			if c.trigger == resumeNetworkTriggerVMGenID {
				if err := vmgenIDWaiter.Wait(); err != nil {
					log.Printf("[guest-agent] resume network VMGenID wait failed: %v", err)
					vmgenIDWaiter.Close()
					armed = false
					continue
				}
				vmgenIDWaiter.Close()
			}
			if err := c.waitAndApplyMailbox(); err != nil {
				log.Printf("[guest-agent] resume network mailbox apply failed: %v", err)
				armed = false
				continue
			}
			log.Printf("[guest-agent] resume network mailbox applied in %s", time.Since(start))
			armed = false
			continue
		}

		if armReady != nil {
			armReady <- nil
		}
		interval := fastInterval
		start := time.Now()
		err := fetchAndApplyResumeNetwork(c.s, c.port, resumeNetworkDialTimeout(interval))
		if err == nil {
			log.Printf("[guest-agent] resume network prearm loop applied in %s", time.Since(start))
			armed = false
			continue
		}

		sleep := interval - time.Since(start)
		if sleep <= 0 {
			continue
		}

		timer := time.NewTimer(sleep)
		select {
		case req := <-c.arm:
			if !timer.Stop() {
				<-timer.C
			}
			applyArm(req)
			req.ready <- nil
		case <-timer.C:
		}
	}
}

func (c *resumeNetworkController) waitAndApplyMailbox() error {
	buf := c.mailbox
	for {
		seq := atomicLoadUint32(buf[resumeNetworkMailboxSeqOffset:])
		if seq == 0 {
			continue
		}

		payloadLen := binary.LittleEndian.Uint32(buf[resumeNetworkMailboxLengthOffset:])
		if payloadLen == 0 || int(payloadLen) > len(buf)-resumeNetworkMailboxPayloadOffset {
			return fmt.Errorf("invalid mailbox payload length %d", payloadLen)
		}

		var payload resumeNetworkPayload
		if err := json.Unmarshal(buf[resumeNetworkMailboxPayloadOffset:resumeNetworkMailboxPayloadOffset+int(payloadLen)], &payload); err != nil {
			return fmt.Errorf("decode mailbox payload: %w", err)
		}
		sendResumeNetworkAck(payload, "mailbox")

		_, err := c.s.ReconfigureNetwork(context.Background(), &pb.ReconfigureNetworkRequest{
			InterfaceName: payload.InterfaceName,
			Mac:           payload.MAC,
			Ipv4:          payload.IPv4,
			Prefix:        payload.Prefix,
			Gateway:       payload.Gateway,
		})
		if err != nil {
			return err
		}
		sendResumeNetworkAck(payload, "applied")
		atomicStoreUint32(buf[resumeNetworkMailboxSeqOffset:], 0)
		return nil
	}
}

func atomicLoadUint32(buf []byte) uint32 {
	return atomic.LoadUint32((*uint32)(unsafe.Pointer(&buf[0])))
}

func atomicStoreUint32(buf []byte, value uint32) {
	atomic.StoreUint32((*uint32)(unsafe.Pointer(&buf[0])), value)
}

func resumeNetworkDialTimeout(interval time.Duration) time.Duration {
	if interval <= 0 {
		return time.Millisecond
	}
	if interval > 10*time.Millisecond {
		return 10 * time.Millisecond
	}
	return interval
}

func (c *resumeNetworkController) Arm(ctx context.Context, interval time.Duration) error {
	req := resumeNetworkArmRequest{
		interval: interval,
		ready:    make(chan error, 1),
	}
	select {
	case c.arm <- req:
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case err := <-req.ready:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *guestServer) ArmResumeNetwork(ctx context.Context, req *pb.ArmResumeNetworkRequest) (*pb.ArmResumeNetworkResponse, error) {
	if s.resumeNetwork == nil {
		return nil, status.Error(codes.FailedPrecondition, "resume network prearm loop is not enabled")
	}

	var interval time.Duration
	if req.PollIntervalMs > 0 {
		interval = time.Duration(req.PollIntervalMs) * time.Millisecond
	}
	if err := s.resumeNetwork.Arm(ctx, interval); err != nil {
		return nil, err
	}
	return &pb.ArmResumeNetworkResponse{}, nil
}

func resumeNetworkVMGenIDLoop(s *guestServer, port uint32) {
	log.Printf("[guest-agent] resume network waiting for VMGenID signal on host port %d", port)
	for {
		if err := waitForVMGenIDResumeSignal(); err != nil {
			log.Printf("[guest-agent] resume network VMGenID wait failed: %v", err)
			time.Sleep(100 * time.Millisecond)
			continue
		}

		start := time.Now()
		if err := fetchAndApplyResumeNetwork(s, port, 50*time.Millisecond); err != nil {
			log.Printf("[guest-agent] resume network VMGenID apply failed: %v", err)
			continue
		}
		log.Printf("[guest-agent] resume network VMGenID applied in %s", time.Since(start))
	}
}

func waitForVMGenIDResumeSignal() error {
	waiter, err := newVMGenIDResumeWaiter()
	if err != nil {
		return err
	}
	defer waiter.Close()
	return waiter.Wait()
}

func newVMGenIDResumeWaiter() (*vmGenIDResumeWaiter, error) {
	f, err := os.Open("/dev/kmsg")
	if err != nil {
		return nil, fmt.Errorf("open /dev/kmsg: %w", err)
	}

	if _, err := f.Seek(0, io.SeekEnd); err != nil {
		log.Printf("[guest-agent] warning: failed to seek /dev/kmsg to end: %v", err)
	}

	return &vmGenIDResumeWaiter{
		file:   f,
		reader: bufio.NewReader(f),
	}, nil
}

func (w *vmGenIDResumeWaiter) Close() {
	if w == nil || w.file == nil {
		return
	}
	_ = w.file.Close()
}

func (w *vmGenIDResumeWaiter) Wait() error {
	for {
		line, err := w.reader.ReadString('\n')
		if err != nil {
			return fmt.Errorf("read /dev/kmsg: %w", err)
		}
		if strings.Contains(line, vmgenIDKmsgSignal) {
			return nil
		}
	}
}

func armResumeNetworkConnection(port uint32) error {
	conn, err := dialHost(port, 5*time.Second)
	if err != nil {
		return fmt.Errorf("dial host port %d: %w", port, err)
	}
	defer conn.Close()

	if _, err := conn.Write([]byte("HELLO\n")); err != nil {
		return fmt.Errorf("write hello: %w", err)
	}
	log.Printf("[guest-agent] resume network watcher armed on host port %d", port)

	var buf [1]byte
	_, err = conn.Read(buf[:])
	if err != nil {
		log.Printf("[guest-agent] resume network watcher woke: %v", err)
		return nil
	}
	return nil
}

func fetchAndApplyResumeNetwork(s *guestServer, port uint32, dialTimeout time.Duration) error {
	conn, err := dialHost(port, dialTimeout)
	if err != nil {
		return fmt.Errorf("dial host config port %d: %w", port, err)
	}
	defer conn.Close()

	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := conn.Write([]byte("FETCH\n")); err != nil {
		return fmt.Errorf("write fetch: %w", err)
	}

	var payload resumeNetworkPayload
	if err := json.NewDecoder(conn).Decode(&payload); err != nil {
		return fmt.Errorf("decode payload: %w", err)
	}

	applyStart := time.Now()
	_, err = s.ReconfigureNetwork(context.Background(), &pb.ReconfigureNetworkRequest{
		InterfaceName: payload.InterfaceName,
		Mac:           payload.MAC,
		Ipv4:          payload.IPv4,
		Prefix:        payload.Prefix,
		Gateway:       payload.Gateway,
	})
	applyElapsed := time.Since(applyStart)
	if err != nil {
		_, _ = fmt.Fprintf(conn, "ERR %s\n", err)
		return err
	}
	sendResumeNetworkAck(payload, "applied")

	writer := bufio.NewWriter(conn)
	_, _ = fmt.Fprintf(writer, "OK apply_ms=%d\n", applyElapsed.Milliseconds())
	_ = writer.Flush()
	return nil
}

func dialHost(port uint32, timeout time.Duration) (*socket.Conn, error) {
	fd, err := unix.Socket(unix.AF_VSOCK, unix.SOCK_STREAM|unix.SOCK_CLOEXEC|unix.SOCK_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}

	if err := unix.Connect(fd, &unix.SockaddrVM{CID: resumeNetworkHostCID, Port: port}); err != nil {
		if !errors.Is(err, unix.EINPROGRESS) && !errors.Is(err, unix.EAGAIN) {
			_ = unix.Close(fd)
			return nil, err
		}
		if err := waitVsockConnect(fd, timeout); err != nil {
			_ = unix.Close(fd)
			return nil, err
		}
	}

	return socket.New(fd, "vsock")
}

func sendResumeNetworkAck(payload resumeNetworkPayload, stage string) {
	if payload.AckPort == 0 || payload.Gateway == "" {
		return
	}

	addr := net.JoinHostPort(payload.Gateway, strconv.FormatUint(uint64(payload.AckPort), 10))
	conn, err := net.DialTimeout("udp4", addr, 100*time.Millisecond)
	if err != nil {
		log.Printf("[guest-agent] resume network ack dial failed: %v", err)
		return
	}
	defer conn.Close()

	_, _ = fmt.Fprintf(conn, "stage=%s mac=%s ip=%s\n", stage, payload.MAC, payload.IPv4)
}

func waitVsockConnect(fd int, timeout time.Duration) error {
	timeoutMS := int(timeout.Milliseconds())
	if timeout > 0 && timeoutMS <= 0 {
		timeoutMS = 1
	}
	ready, err := unix.Poll([]unix.PollFd{{
		Fd:     int32(fd),
		Events: unix.POLLOUT,
	}}, timeoutMS)
	if err != nil {
		return err
	}
	if ready == 0 {
		return context.DeadlineExceeded
	}

	errno, err := unix.GetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_ERROR)
	if err != nil {
		return err
	}
	if errno != 0 {
		return unix.Errno(errno)
	}
	return nil
}
