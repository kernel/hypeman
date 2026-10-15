package guest

import (
	"context"
	"encoding/binary"
	"io"
	"net"
	"testing"

	"github.com/stretchr/testify/require"
)

type pipeDialer struct {
	serve func(net.Conn)
	port  int
}

func (d *pipeDialer) DialVsock(_ context.Context, port int) (net.Conn, error) {
	d.port = port
	host, agent := net.Pipe()
	go d.serve(agent)
	return host, nil
}

func (d *pipeDialer) Key() string { return "pipe" }

func answerPortForward(status byte, gotPort *uint16) func(net.Conn) {
	return func(agent net.Conn) {
		defer agent.Close()
		if err := binary.Read(agent, binary.BigEndian, gotPort); err != nil {
			return
		}
		if _, err := agent.Write([]byte{status}); err != nil || status != PortForwardStatusOK {
			return
		}
		_, _ = io.Copy(agent, agent)
	}
}

func TestDialPortHandshake(t *testing.T) {
	var gotPort uint16
	dialer := &pipeDialer{serve: answerPortForward(PortForwardStatusOK, &gotPort)}
	conn, err := DialPort(context.Background(), dialer, 9222)
	require.NoError(t, err)
	defer conn.Close()
	require.Equal(t, PortForwardVsockPort, dialer.port)
	require.EqualValues(t, 9222, gotPort)

	_, err = conn.Write([]byte("ping"))
	require.NoError(t, err)
	buf := make([]byte, 4)
	_, err = io.ReadFull(conn, buf)
	require.NoError(t, err)
	require.Equal(t, "ping", string(buf))
}

func TestDialPortReportsUnavailablePort(t *testing.T) {
	var gotPort uint16
	dialer := &pipeDialer{serve: answerPortForward(PortForwardStatusDialFailed, &gotPort)}
	_, err := DialPort(context.Background(), dialer, 9222)
	require.ErrorIs(t, err, ErrGuestPortUnavailable)
}

func TestDialPortRejectsPortZero(t *testing.T) {
	_, err := DialPort(context.Background(), &pipeDialer{}, 0)
	require.Error(t, err)
}
