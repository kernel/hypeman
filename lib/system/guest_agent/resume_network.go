//go:build linux

package main

import (
	"bufio"
	"context"
	"encoding/binary"
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
	"github.com/kernel/hypeman/lib/mailbox"
	"golang.org/x/sys/unix"
)

const vmgenIDKmsgSignal = "crng reseeded due to virtual machine fork"
const resumeNetworkMailboxPayloadTimeout = 5 * time.Second

type vmGenIDResumeWaiter struct {
	file   *os.File
	reader *bufio.Reader
}

func startResumeNetworkWatcher(s *guestServer) {
	if strings.TrimSpace(os.Getenv(mailbox.MailboxEnv)) != "1" {
		return
	}

	mailbox := newResumeNetworkMailbox()
	if mailbox == nil {
		return
	}

	go resumeNetworkMailboxLoop(s, mailbox)
}

func newResumeNetworkMailbox() []byte {
	token := strings.TrimSpace(os.Getenv(mailbox.MailboxTokenEnv))
	if !mailbox.ValidToken(token) {
		log.Printf("[guest-agent] resume network mailbox disabled: invalid %s", mailbox.MailboxTokenEnv)
		return nil
	}

	buf := make([]byte, mailbox.MailboxSize)
	copy(buf, mailbox.MailboxMagic)
	copy(buf[len(mailbox.MailboxMagic):mailbox.MailboxSeqOffset], token)
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
	return waitAndApplyResumeNetworkMailboxWithTimeout(s, buf, resumeNetworkMailboxPayloadTimeout)
}

func waitAndApplyResumeNetworkMailboxWithTimeout(s *guestServer, buf []byte, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		seq := atomicLoadUint32(buf[mailbox.MailboxSeqOffset:])
		if seq == 0 {
			if time.Now().After(deadline) {
				return fmt.Errorf("resume network mailbox payload was not patched within %s", timeout)
			}
			time.Sleep(100 * time.Microsecond)
			continue
		}

		payloadLen := binary.LittleEndian.Uint32(buf[mailbox.MailboxLengthOffset:])
		payload, err := mailbox.DecodePayloadFrame(buf, payloadLen)
		if err != nil {
			return err
		}

		_, err = s.ReconfigureNetwork(context.Background(), &pb.ReconfigureNetworkRequest{
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
		atomicStoreUint32(buf[mailbox.MailboxSeqOffset:], 0)
		return nil
	}
}

func sendResumeNetworkAck(payload mailbox.Payload, stage string) {
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
