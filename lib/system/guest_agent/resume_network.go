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
const resumeNetworkDebugStagesEnv = "HYPEMAN_RESUME_NETWORK_DEBUG_STAGES"
const vmgenIDKmsgSignal = "crng reseeded due to virtual machine fork"
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

type vmGenIDResumeWaiter struct {
	file   *os.File
	reader *bufio.Reader
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
		waiter, err := newVMGenIDResumeWaiter()
		if err != nil {
			log.Printf("[guest-agent] resume network VMGenID prepare failed: %v", err)
			time.Sleep(100 * time.Millisecond)
			continue
		}

		start := time.Now()
		if err := waiter.Wait(); err != nil {
			waiter.Close()
			log.Printf("[guest-agent] resume network VMGenID wait failed: %v", err)
			time.Sleep(100 * time.Millisecond)
			continue
		}
		waiter.Close()

		if err := waitAndApplyResumeNetworkMailbox(s, mailbox); err != nil {
			log.Printf("[guest-agent] resume network mailbox apply failed: %v", err)
			time.Sleep(25 * time.Millisecond)
			continue
		}
		log.Printf("[guest-agent] resume network mailbox applied in %s", time.Since(start))
	}
}

func waitAndApplyResumeNetworkMailbox(s *guestServer, buf []byte) error {
	signalSeen := time.Now()
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

		debugStages := strings.TrimSpace(os.Getenv(resumeNetworkDebugStagesEnv)) == "1"
		if debugStages {
			elapsed := time.Since(signalSeen).Microseconds()
			sendResumeNetworkAck(payload, "signal_seen", fmt.Sprintf("guest_signal_to_mailbox_us=%d", elapsed))
			sendResumeNetworkAck(payload, "mailbox_seen", fmt.Sprintf("guest_signal_to_mailbox_us=%d", elapsed))
			sendResumeNetworkAck(payload, "netlink_start", fmt.Sprintf("guest_signal_to_netlink_start_us=%d", time.Since(signalSeen).Microseconds()))
		}
		_, err := s.ReconfigureNetwork(context.Background(), &pb.ReconfigureNetworkRequest{
			InterfaceName: payload.InterfaceName,
			Mac:           payload.MAC,
			Ipv4:          payload.IPv4,
			Prefix:        payload.Prefix,
			Gateway:       payload.Gateway,
		})
		if err != nil {
			if debugStages {
				sendResumeNetworkAck(payload, "netlink_error", fmt.Sprintf("guest_signal_to_netlink_error_us=%d", time.Since(signalSeen).Microseconds()))
			}
			return err
		}
		if debugStages {
			sendResumeNetworkAck(payload, "netlink_done", fmt.Sprintf("guest_signal_to_netlink_done_us=%d", time.Since(signalSeen).Microseconds()))
		}
		sendResumeNetworkAck(payload, "applied", fmt.Sprintf("guest_signal_to_applied_us=%d", time.Since(signalSeen).Microseconds()))
		atomicStoreUint32(buf[resumeNetworkMailboxSeqOffset:], 0)
		return nil
	}
}

func sendResumeNetworkAck(payload resumeNetworkPayload, stage string, fields ...string) {
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

	extra := ""
	if len(fields) > 0 {
		extra = " " + strings.Join(fields, " ")
	}
	_, _ = fmt.Fprintf(conn, "stage=%s mac=%s ip=%s%s\n", stage, payload.MAC, payload.IPv4, extra)
}

func atomicLoadUint32(buf []byte) uint32 {
	return atomic.LoadUint32((*uint32)(unsafe.Pointer(&buf[0])))
}

func atomicStoreUint32(buf []byte, value uint32) {
	atomic.StoreUint32((*uint32)(unsafe.Pointer(&buf[0])), value)
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
