package mailbox

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"regexp"
	"sync"

	"golang.org/x/sys/unix"
)

const MailboxEnv = "HYPEMAN_RESUME_NETWORK_MAILBOX"
const MailboxTokenEnv = "HYPEMAN_RESUME_NETWORK_MAILBOX_TOKEN"

const MailboxSize = 4096
const MailboxMagic = "HYPEMAN_RESUME_NETWORK_MAILBOX_V1\x00"
const MailboxSeqOffset = 64
const MailboxLengthOffset = 68
const MailboxPayloadOffset = 72
const MailboxTokenMaxLen = MailboxSeqOffset - len(MailboxMagic)

const ForkMailboxSize = 4096
const ForkMailboxMagic = "HYPEMAN_FORK_MAILBOX_V1\x00"
const ForkMailboxSeqOffset = 256
const ForkMailboxLengthOffset = 260
const ForkMailboxPayloadOffset = 264
const ForkMailboxPayloadSize = ForkMailboxSize - ForkMailboxPayloadOffset
const ForkMailboxTokenMaxLen = 128

var ForkMailboxNamePattern = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,64}$`)

type Layout struct {
	Size          int
	SeqOffset     int
	LengthOffset  int
	PayloadOffset int
}

var ResumeNetworkLayout = Layout{
	Size:          MailboxSize,
	SeqOffset:     MailboxSeqOffset,
	LengthOffset:  MailboxLengthOffset,
	PayloadOffset: MailboxPayloadOffset,
}

var ForkLayout = Layout{
	Size:          ForkMailboxSize,
	SeqOffset:     ForkMailboxSeqOffset,
	LengthOffset:  ForkMailboxLengthOffset,
	PayloadOffset: ForkMailboxPayloadOffset,
}

func (l Layout) PayloadSize() int {
	return l.Size - l.PayloadOffset
}

type Payload struct {
	InterfaceName string `json:"interface_name"`
	MAC           string `json:"mac"`
	IPv4          string `json:"ipv4"`
	Prefix        uint32 `json:"prefix"`
	Gateway       string `json:"gateway"`
	AckPort       uint32 `json:"ack_port,omitempty"`
}

func ValidToken(token string) bool {
	return token != "" && len(token) <= MailboxTokenMaxLen
}

func Marker(token string) ([]byte, error) {
	if !ValidToken(token) {
		return nil, fmt.Errorf("resume network mailbox token is invalid")
	}
	marker := make([]byte, 0, len(MailboxMagic)+len(token))
	marker = append(marker, MailboxMagic...)
	marker = append(marker, token...)
	return marker, nil
}

func ValidForkMailboxName(name string) bool {
	return ForkMailboxNamePattern.MatchString(name)
}

func ValidForkMailboxToken(token string) bool {
	return token != "" && len(token) <= ForkMailboxTokenMaxLen
}

func ForkMailboxMarker(name, token string) ([]byte, error) {
	if !ValidForkMailboxName(name) {
		return nil, fmt.Errorf("fork mailbox name is invalid")
	}
	if !ValidForkMailboxToken(token) {
		return nil, fmt.Errorf("fork mailbox token is invalid")
	}
	marker := make([]byte, 0, len(ForkMailboxMagic)+len(name)+1+len(token))
	marker = append(marker, ForkMailboxMagic...)
	marker = append(marker, name...)
	marker = append(marker, 0)
	marker = append(marker, token...)
	if len(marker) > ForkMailboxSeqOffset {
		return nil, fmt.Errorf("fork mailbox marker is too long")
	}
	return marker, nil
}

func WriteForkMailboxPayloadAt(w io.WriterAt, offset int64, payload []byte) error {
	return WritePayloadAt(w, ForkLayout, offset, payload)
}

func FindMarker(file *os.File, size int64, marker []byte, cache *sync.Map) (int64, error) {
	cacheKey := string(marker)
	if cache != nil {
		if cached, ok := cache.Load(cacheKey); ok {
			if offset, ok := cached.(int64); ok && offset >= 0 && offset+int64(len(marker)) <= size {
				buf := make([]byte, len(marker))
				if _, err := file.ReadAt(buf, offset); err == nil && bytes.Equal(buf, marker) {
					return offset, nil
				}
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
	if cache != nil {
		cache.Store(cacheKey, int64(idx))
	}
	return int64(idx), nil
}

func EnsurePayloadFits(layout Layout, memorySize int64, offset int64, payloadLen int) error {
	if payloadLen > layout.PayloadSize() {
		return fmt.Errorf("mailbox payload too large: %d bytes", payloadLen)
	}
	if offset+int64(layout.PayloadOffset)+int64(payloadLen) > memorySize {
		return fmt.Errorf("mailbox marker is too close to end of memory file")
	}
	return nil
}

func WritePayloadAt(w io.WriterAt, layout Layout, offset int64, payload []byte) error {
	if len(payload) > layout.PayloadSize() {
		return fmt.Errorf("mailbox payload too large: %d bytes", len(payload))
	}
	if _, err := w.WriteAt(payload, offset+int64(layout.PayloadOffset)); err != nil {
		return fmt.Errorf("write mailbox payload: %w", err)
	}
	var u32 [4]byte
	binary.LittleEndian.PutUint32(u32[:], uint32(len(payload)))
	if _, err := w.WriteAt(u32[:], offset+int64(layout.LengthOffset)); err != nil {
		return fmt.Errorf("write mailbox payload length: %w", err)
	}
	binary.LittleEndian.PutUint32(u32[:], 1)
	if _, err := w.WriteAt(u32[:], offset+int64(layout.SeqOffset)); err != nil {
		return fmt.Errorf("write mailbox sequence: %w", err)
	}
	return nil
}

func MarshalPayload(payload *Payload) ([]byte, error) {
	if payload == nil {
		return nil, fmt.Errorf("resume network mailbox payload is nil")
	}
	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal resume network mailbox payload: %w", err)
	}
	if len(payloadBytes) > MailboxSize-MailboxPayloadOffset {
		return nil, fmt.Errorf("resume network mailbox payload too large: %d bytes", len(payloadBytes))
	}
	return payloadBytes, nil
}

func DecodePayloadFrame(buf []byte, payloadLen uint32) (Payload, error) {
	if payloadLen == 0 || int(payloadLen) > len(buf)-MailboxPayloadOffset {
		return Payload{}, fmt.Errorf("invalid mailbox payload length %d", payloadLen)
	}
	var payload Payload
	if err := json.Unmarshal(buf[MailboxPayloadOffset:MailboxPayloadOffset+int(payloadLen)], &payload); err != nil {
		return Payload{}, fmt.Errorf("decode mailbox payload: %w", err)
	}
	return payload, nil
}
