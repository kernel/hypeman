//go:build linux

package autostandby

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"sync"
	"syscall"
	"time"

	"github.com/vishvananda/netlink/nl"
	"golang.org/x/sys/unix"
)

type conntrackStream struct {
	fd        int
	closeOnce sync.Once
	events    chan ConnectionEvent
	errs      chan error
}

var errUnsupportedConntrackProtocol = errors.New("unsupported conntrack protocol")

// OpenStream subscribes to IPv4 conntrack NEW, UPDATE, and DESTROY events.
func (s *ConntrackSource) OpenStream(ctx context.Context) (ConnectionStream, error) {
	fd, err := unix.Socket(unix.AF_NETLINK, unix.SOCK_RAW, unix.NETLINK_NETFILTER)
	if err != nil {
		return nil, fmt.Errorf("open netfilter netlink socket: %w", err)
	}

	closeFD := func() { _ = unix.Close(fd) }
	if err := unix.Bind(fd, &unix.SockaddrNetlink{Family: unix.AF_NETLINK}); err != nil {
		closeFD()
		return nil, fmt.Errorf("bind netfilter netlink socket: %w", err)
	}
	if err := unix.SetsockoptInt(fd, unix.SOL_NETLINK, unix.NETLINK_ADD_MEMBERSHIP, unix.NFNLGRP_CONNTRACK_NEW); err != nil {
		closeFD()
		return nil, fmt.Errorf("subscribe conntrack new events: %w", err)
	}
	if err := unix.SetsockoptInt(fd, unix.SOL_NETLINK, unix.NETLINK_ADD_MEMBERSHIP, unix.NFNLGRP_CONNTRACK_UPDATE); err != nil {
		closeFD()
		return nil, fmt.Errorf("subscribe conntrack update events: %w", err)
	}
	if err := unix.SetsockoptInt(fd, unix.SOL_NETLINK, unix.NETLINK_ADD_MEMBERSHIP, unix.NFNLGRP_CONNTRACK_DESTROY); err != nil {
		closeFD()
		return nil, fmt.Errorf("subscribe conntrack destroy events: %w", err)
	}

	stream := &conntrackStream{
		fd:     fd,
		events: make(chan ConnectionEvent, 256),
		errs:   make(chan error, 16),
	}
	go stream.run(ctx)
	return stream, nil
}

func (s *conntrackStream) Events() <-chan ConnectionEvent { return s.events }

func (s *conntrackStream) Errors() <-chan error { return s.errs }

func (s *conntrackStream) Close() error {
	var err error
	s.closeOnce.Do(func() {
		err = unix.Close(s.fd)
	})
	return err
}

func (s *conntrackStream) run(ctx context.Context) {
	defer close(s.events)
	defer close(s.errs)

	go func() {
		<-ctx.Done()
		_ = s.Close()
	}()

	buf := make([]byte, 1<<20)
	for {
		n, _, err := unix.Recvfrom(s.fd, buf, 0)
		if err != nil {
			if ctx.Err() != nil || err == unix.EBADF || err == syscall.ENOTCONN {
				return
			}
			select {
			case s.errs <- fmt.Errorf("recv conntrack events: %w", err):
			default:
			}
			return
		}

		msgs, err := syscall.ParseNetlinkMessage(buf[:n])
		if err != nil {
			select {
			case s.errs <- fmt.Errorf("parse conntrack netlink messages: %w", err):
			default:
			}
			continue
		}

		for _, msg := range msgs {
			event, ok, err := connectionEventFromNetlinkMessage(msg)
			if err != nil {
				if errors.Is(err, errUnsupportedConntrackProtocol) {
					continue
				}
				select {
				case s.errs <- err:
				default:
				}
				continue
			}
			if !ok {
				continue
			}
			event.ObservedAt = time.Now().UTC()
			select {
			case s.events <- event:
			case <-ctx.Done():
				return
			}
		}
	}
}

func connectionEventFromNetlinkMessage(msg syscall.NetlinkMessage) (ConnectionEvent, bool, error) {
	switch msg.Header.Type {
	case unix.NLMSG_NOOP, unix.NLMSG_DONE:
		return ConnectionEvent{}, false, nil
	case unix.NLMSG_ERROR:
		if len(msg.Data) >= 4 {
			errno := -int32(nl.NativeEndian().Uint32(msg.Data[:4]))
			if errno != 0 {
				return ConnectionEvent{}, false, unix.Errno(errno)
			}
		}
		return ConnectionEvent{}, false, nil
	}

	if len(msg.Data) < nl.SizeofNfgenmsg {
		return ConnectionEvent{}, false, nil
	}
	if msg.Data[0] != unix.AF_INET {
		return ConnectionEvent{}, false, nil
	}

	var eventType ConnectionEventType
	switch int(msg.Header.Type & 0x00ff) {
	case nl.IPCTNL_MSG_CT_NEW:
		eventType = ConnectionEventNew
	case nl.IPCTNL_MSG_CT_DELETE:
		eventType = ConnectionEventDestroy
	default:
		return ConnectionEvent{}, false, nil
	}

	conn, ok, err := connectionFromRawData(msg.Data)
	if err != nil || !ok {
		return ConnectionEvent{}, false, err
	}
	return ConnectionEvent{Type: eventType, Connection: conn}, true, nil
}

func connectionFromRawData(data []byte) (Connection, bool, error) {
	if len(data) < nl.SizeofNfgenmsg {
		return Connection{}, false, nil
	}

	reader := bytes.NewReader(data[nl.SizeofNfgenmsg:])
	conn := Connection{}
	for reader.Len() > 0 {
		attr, err := readNfAttr(reader)
		if err != nil {
			return Connection{}, false, err
		}
		switch attr.typ {
		case nl.CTA_TUPLE_ORIG:
			if err := parseTuple(attr.payload, &conn); err != nil {
				return Connection{}, false, err
			}
		case nl.CTA_PROTOINFO:
			if err := parseProtoInfo(attr.payload, &conn); err != nil {
				return Connection{}, false, err
			}
		}
	}

	if !conn.OriginalSourceIP.IsValid() || !conn.OriginalDestinationIP.IsValid() {
		return Connection{}, false, nil
	}
	return conn, true, nil
}

type nfAttr struct {
	typ     uint16
	nested  bool
	payload []byte
}

func readNfAttr(reader *bytes.Reader) (nfAttr, error) {
	var rawLen uint16
	var rawType uint16
	if err := binary.Read(reader, nl.NativeEndian(), &rawLen); err != nil {
		return nfAttr{}, err
	}
	if err := binary.Read(reader, nl.NativeEndian(), &rawType); err != nil {
		return nfAttr{}, err
	}
	if rawLen < nl.SizeofNfattr {
		return nfAttr{}, fmt.Errorf("invalid netfilter attribute length %d", rawLen)
	}

	payloadLen := int(rawLen) - nl.SizeofNfattr
	payload := make([]byte, payloadLen)
	if _, err := reader.Read(payload); err != nil {
		return nfAttr{}, err
	}

	padding := nlaAlignedLen(rawLen) - rawLen
	if padding > 0 {
		if _, err := reader.Seek(int64(padding), 1); err != nil {
			return nfAttr{}, err
		}
	}

	return nfAttr{
		typ:     rawType & nl.NLA_TYPE_MASK,
		nested:  rawType&nl.NLA_F_NESTED == nl.NLA_F_NESTED,
		payload: payload,
	}, nil
}

func parseTuple(payload []byte, conn *Connection) error {
	reader := bytes.NewReader(payload)
	for reader.Len() > 0 {
		attr, err := readNfAttr(reader)
		if err != nil {
			return err
		}
		switch attr.typ {
		case nl.CTA_TUPLE_IP:
			if err := parseTupleIP(attr.payload, conn); err != nil {
				return err
			}
		case nl.CTA_TUPLE_PROTO:
			if err := parseTupleProto(attr.payload, conn); err != nil {
				return err
			}
		}
	}
	return nil
}

func parseTupleIP(payload []byte, conn *Connection) error {
	reader := bytes.NewReader(payload)
	for reader.Len() > 0 {
		attr, err := readNfAttr(reader)
		if err != nil {
			return err
		}
		switch attr.typ {
		case nl.CTA_IP_V4_SRC:
			addr, ok := addrFromIP(attr.payload)
			if ok {
				conn.OriginalSourceIP = addr
			}
		case nl.CTA_IP_V4_DST:
			addr, ok := addrFromIP(attr.payload)
			if ok {
				conn.OriginalDestinationIP = addr
			}
		}
	}
	return nil
}

func parseTupleProto(payload []byte, conn *Connection) error {
	reader := bytes.NewReader(payload)
	var protocol uint8
	for reader.Len() > 0 {
		attr, err := readNfAttr(reader)
		if err != nil {
			return err
		}
		switch attr.typ {
		case nl.CTA_PROTO_NUM:
			if len(attr.payload) > 0 {
				protocol = attr.payload[0]
			}
		case nl.CTA_PROTO_SRC_PORT:
			if len(attr.payload) >= 2 {
				conn.OriginalSourcePort = binary.BigEndian.Uint16(attr.payload[:2])
			}
		case nl.CTA_PROTO_DST_PORT:
			if len(attr.payload) >= 2 {
				conn.OriginalDestinationPort = binary.BigEndian.Uint16(attr.payload[:2])
			}
		}
	}
	if protocol != unix.IPPROTO_TCP {
		return fmt.Errorf("%w %d", errUnsupportedConntrackProtocol, protocol)
	}
	return nil
}

func parseProtoInfo(payload []byte, conn *Connection) error {
	reader := bytes.NewReader(payload)
	for reader.Len() > 0 {
		attr, err := readNfAttr(reader)
		if err != nil {
			return err
		}
		if attr.typ != nl.CTA_PROTOINFO_TCP {
			continue
		}

		tcpReader := bytes.NewReader(attr.payload)
		for tcpReader.Len() > 0 {
			tcpAttr, err := readNfAttr(tcpReader)
			if err != nil {
				return err
			}
			if tcpAttr.typ == nl.CTA_PROTOINFO_TCP_STATE && len(tcpAttr.payload) > 0 {
				conn.TCPState = TCPState(tcpAttr.payload[0])
			}
		}
	}
	return nil
}

func nlaAlignedLen(length uint16) uint16 {
	return (length + nl.NLA_ALIGNTO - 1) & ^(nl.NLA_ALIGNTO - 1)
}
