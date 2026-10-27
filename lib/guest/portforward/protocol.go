// Package portforward is the handshake between the host and the guest agent's port
// forwarder. It has no dependencies, so the guest agent can be built with it alone.
package portforward

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// VsockPort is the guest vsock port where the agent accepts forwards.
const VsockPort = 2223

// Status bytes the agent answers with before copying bytes both ways.
const (
	StatusOK         byte = 0
	StatusDialFailed byte = 1
)

// ErrUnknownStatus means the agent answered with a byte that is not a known status.
var ErrUnknownStatus = errors.New("unknown port forward status")

// WriteRequest sends the target port as two big-endian bytes.
func WriteRequest(w io.Writer, port uint16) error {
	return binary.Write(w, binary.BigEndian, port)
}

// ReadRequest reads the target port the host sent.
func ReadRequest(r io.Reader) (uint16, error) {
	var port uint16
	err := binary.Read(r, binary.BigEndian, &port)
	return port, err
}

// WriteStatus sends the agent's answer.
func WriteStatus(w io.Writer, status byte) error {
	_, err := w.Write([]byte{status})
	return err
}

// ReadStatus reads the agent's answer and rejects bytes other than the known statuses.
func ReadStatus(r io.Reader) (byte, error) {
	var status [1]byte
	if _, err := io.ReadFull(r, status[:]); err != nil {
		return 0, err
	}
	if status[0] != StatusOK && status[0] != StatusDialFailed {
		return 0, fmt.Errorf("%w %d", ErrUnknownStatus, status[0])
	}
	return status[0], nil
}
