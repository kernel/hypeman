package mailbox

import (
	"encoding/json"
	"fmt"
)

const MailboxEnv = "HYPEMAN_RESUME_NETWORK_MAILBOX"
const MailboxTokenEnv = "HYPEMAN_RESUME_NETWORK_MAILBOX_TOKEN"

const MailboxSize = 4096
const MailboxMagic = "HYPEMAN_RESUME_NETWORK_MAILBOX_V1\x00"
const MailboxSeqOffset = 64
const MailboxLengthOffset = 68
const MailboxPayloadOffset = 72
const MailboxTokenMaxLen = MailboxSeqOffset - len(MailboxMagic)

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
