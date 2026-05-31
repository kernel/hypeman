//go:build linux

package main

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/json"
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
	"golang.org/x/sys/unix"
)

const resumeNetworkMailboxEnv = "HYPEMAN_RESUME_NETWORK_MAILBOX"
const resumeNetworkMailboxTokenEnv = "HYPEMAN_RESUME_NETWORK_MAILBOX_TOKEN"
const resumeNetworkSignalEnv = "HYPEMAN_RESUME_NETWORK_SIGNAL"
const resumeNetworkAckStagesEnv = "HYPEMAN_RESUME_NETWORK_ACK_STAGES"
const vmgenIDKmsgSignal = "crng reseeded due to virtual machine fork"
const resumeNetworkMailboxSize = 4096
const resumeNetworkMailboxSeqOffset = 64
const resumeNetworkMailboxLengthOffset = 68
const resumeNetworkMailboxPayloadOffset = 72
const vmClockDevicePath = "/dev/vmclock0"

const vmClockABISize = 112
const vmClockMagic = 0x4b4c4356
const vmClockFlagsOffset = 24
const vmClockGenerationCounterOffset = 104
const vmClockFlagGenerationCounterPresent = 1 << 8
const vmClockFlagNotificationPresent = 1 << 9

var resumeNetworkMailboxMagic = []byte("HYPEMAN_RESUME_NETWORK_MAILBOX_V1\x00")

type resumeNetworkPayload struct {
	InterfaceName string `json:"interface_name"`
	MAC           string `json:"mac"`
	IPv4          string `json:"ipv4"`
	Prefix        uint32 `json:"prefix"`
	Gateway       string `json:"gateway"`
	AckPort       uint32 `json:"ack_port,omitempty"`
}

type vmGenIDResumeWaiter struct {
	file   *os.File
	reader *bufio.Reader
}

type vmClockResumeWaiter struct {
	file       *os.File
	generation uint64
}

type vmClockSpinResumeWaiter struct {
	file       *os.File
	data       []byte
	generation uint64
}

type resumeNetworkWaiter interface {
	Close()
	Name() string
	Wait() error
}

func startResumeNetworkWatcher(s *guestServer) {
	if strings.TrimSpace(os.Getenv(resumeNetworkMailboxEnv)) != "1" {
		return
	}

	mailbox := newResumeNetworkMailbox()
	if mailbox == nil {
		return
	}

	go resumeNetworkMailboxLoop(s, mailbox)
}

func newResumeNetworkMailbox() []byte {
	token := strings.TrimSpace(os.Getenv(resumeNetworkMailboxTokenEnv))
	if token == "" {
		log.Printf("[guest-agent] resume network mailbox disabled: missing %s", resumeNetworkMailboxTokenEnv)
		return nil
	}
	if len(token) > resumeNetworkMailboxSeqOffset-len(resumeNetworkMailboxMagic) {
		log.Printf("[guest-agent] resume network mailbox disabled: %s is too long", resumeNetworkMailboxTokenEnv)
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

func resumeNetworkMailboxLoop(s *guestServer, mailbox []byte) {
	for {
		waiter, err := newResumeNetworkWaiter()
		if err != nil {
			log.Printf("[guest-agent] resume network signal prepare failed: %v", err)
			time.Sleep(100 * time.Millisecond)
			continue
		}

		start := time.Now()
		if err := waiter.Wait(); err != nil {
			name := waiter.Name()
			waiter.Close()
			log.Printf("[guest-agent] resume network %s wait failed: %v", name, err)
			time.Sleep(100 * time.Millisecond)
			continue
		}
		name := waiter.Name()
		waiter.Close()

		if err := waitAndApplyResumeNetworkMailbox(s, mailbox); err != nil {
			log.Printf("[guest-agent] resume network mailbox apply failed: %v", err)
			time.Sleep(25 * time.Millisecond)
			continue
		}
		log.Printf("[guest-agent] resume network mailbox applied after %s signal in %s", name, time.Since(start))
	}
}

func waitAndApplyResumeNetworkMailbox(s *guestServer, buf []byte) error {
	for {
		seq := atomicLoadUint32(buf[resumeNetworkMailboxSeqOffset:])
		if seq == 0 {
			time.Sleep(100 * time.Microsecond)
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

		if strings.TrimSpace(os.Getenv(resumeNetworkAckStagesEnv)) == "1" {
			sendResumeNetworkAck(payload, "mailbox")
		}

		_, err := s.ReconfigureNetwork(context.Background(), &pb.ReconfigureNetworkRequest{
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

func atomicLoadUint32(buf []byte) uint32 {
	return atomic.LoadUint32((*uint32)(unsafe.Pointer(&buf[0])))
}

func atomicStoreUint32(buf []byte, value uint32) {
	atomic.StoreUint32((*uint32)(unsafe.Pointer(&buf[0])), value)
}

func newResumeNetworkWaiter() (resumeNetworkWaiter, error) {
	signal := strings.ToLower(strings.TrimSpace(os.Getenv(resumeNetworkSignalEnv)))
	switch signal {
	case "", "auto":
		waiter, err := newVMClockResumeWaiter()
		if err == nil {
			return waiter, nil
		}
		log.Printf("[guest-agent] resume network VMClock unavailable, falling back to VMGenID: %v", err)
		return newVMGenIDResumeWaiter()
	case "vmclock":
		return newVMClockResumeWaiter()
	case "vmclock-spin":
		return newVMClockSpinResumeWaiter()
	case "vmgenid":
		return newVMGenIDResumeWaiter()
	default:
		return nil, fmt.Errorf("unknown %s value %q", resumeNetworkSignalEnv, signal)
	}
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

func (w *vmGenIDResumeWaiter) Name() string {
	return "vmgenid"
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

func newVMClockResumeWaiter() (*vmClockResumeWaiter, error) {
	f, err := os.Open(vmClockDevicePath)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", vmClockDevicePath, err)
	}

	generation, err := readVMClockGeneration(f)
	if err != nil {
		_ = f.Close()
		return nil, err
	}

	return &vmClockResumeWaiter{
		file:       f,
		generation: generation,
	}, nil
}

func (w *vmClockResumeWaiter) Name() string {
	return "vmclock"
}

func (w *vmClockResumeWaiter) Close() {
	if w == nil || w.file == nil {
		return
	}
	_ = w.file.Close()
}

func (w *vmClockResumeWaiter) Wait() error {
	for {
		fds := []unix.PollFd{{
			Fd:     int32(w.file.Fd()),
			Events: unix.POLLIN,
		}}
		_, err := unix.Poll(fds, -1)
		if err == unix.EINTR {
			continue
		}
		if err != nil {
			return fmt.Errorf("poll %s: %w", vmClockDevicePath, err)
		}
		if fds[0].Revents&unix.POLLHUP != 0 {
			return fmt.Errorf("%s does not support notifications", vmClockDevicePath)
		}
		if fds[0].Revents&unix.POLLIN == 0 {
			continue
		}

		generation, err := readVMClockGeneration(w.file)
		if err != nil {
			return err
		}
		if generation != w.generation {
			w.generation = generation
			return nil
		}
	}
}

func readVMClockGeneration(f *os.File) (uint64, error) {
	generation, flags, err := readVMClockState(f)
	if err != nil {
		return 0, err
	}
	if flags&vmClockFlagNotificationPresent == 0 {
		return 0, fmt.Errorf("%s missing notification support", vmClockDevicePath)
	}
	return generation, nil
}

func newVMClockSpinResumeWaiter() (*vmClockSpinResumeWaiter, error) {
	f, err := os.Open(vmClockDevicePath)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", vmClockDevicePath, err)
	}

	generation, _, err := readVMClockState(f)
	if err != nil {
		_ = f.Close()
		return nil, err
	}

	data, err := unix.Mmap(int(f.Fd()), 0, vmClockABISize, unix.PROT_READ, unix.MAP_SHARED)
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("mmap %s: %w", vmClockDevicePath, err)
	}

	return &vmClockSpinResumeWaiter{
		file:       f,
		data:       data,
		generation: generation,
	}, nil
}

func (w *vmClockSpinResumeWaiter) Name() string {
	return "vmclock-spin"
}

func (w *vmClockSpinResumeWaiter) Close() {
	if w == nil {
		return
	}
	if w.data != nil {
		_ = unix.Munmap(w.data)
		w.data = nil
	}
	if w.file != nil {
		_ = w.file.Close()
	}
}

func (w *vmClockSpinResumeWaiter) Wait() error {
	if len(w.data) < vmClockGenerationCounterOffset+8 {
		return fmt.Errorf("mmap %s: short mapping %d", vmClockDevicePath, len(w.data))
	}
	ptr := (*uint64)(unsafe.Pointer(&w.data[vmClockGenerationCounterOffset]))
	for {
		generation := atomic.LoadUint64(ptr)
		if generation != w.generation {
			w.generation = generation
			return nil
		}
	}
}

func readVMClockState(f *os.File) (uint64, uint64, error) {
	buf := make([]byte, vmClockABISize)
	n, err := unix.Pread(int(f.Fd()), buf, 0)
	if err != nil {
		return 0, 0, fmt.Errorf("read %s: %w", vmClockDevicePath, err)
	}
	if n < vmClockABISize {
		return 0, 0, fmt.Errorf("read %s: short read %d", vmClockDevicePath, n)
	}

	magic := binary.LittleEndian.Uint32(buf[0:4])
	if magic != vmClockMagic {
		return 0, 0, fmt.Errorf("read %s: invalid magic 0x%x", vmClockDevicePath, magic)
	}

	flags := binary.LittleEndian.Uint64(buf[vmClockFlagsOffset : vmClockFlagsOffset+8])
	if flags&vmClockFlagGenerationCounterPresent == 0 {
		return 0, 0, fmt.Errorf("%s missing generation counter support", vmClockDevicePath)
	}

	return binary.LittleEndian.Uint64(buf[vmClockGenerationCounterOffset : vmClockGenerationCounterOffset+8]), flags, nil
}
