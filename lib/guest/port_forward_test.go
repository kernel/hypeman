package guest

import (
	"context"
	"encoding/binary"
	"io"
	"net"
	"testing"
	"time"

	"github.com/kernel/hypeman/lib/guest/portforward"
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
		if _, err := agent.Write([]byte{status}); err != nil || status != portforward.StatusOK {
			return
		}
		_, _ = io.Copy(agent, agent)
	}
}

func TestDialPortHandshake(t *testing.T) {
	var gotPort uint16
	dialer := &pipeDialer{serve: answerPortForward(portforward.StatusOK, &gotPort)}
	conn, err := DialPort(context.Background(), dialer, 9222)
	require.NoError(t, err)
	defer conn.Close()
	require.Equal(t, portforward.VsockPort, dialer.port)
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
	dialer := &pipeDialer{serve: answerPortForward(portforward.StatusDialFailed, &gotPort)}
	_, err := DialPort(context.Background(), dialer, 9222)
	require.ErrorIs(t, err, ErrGuestPortUnavailable)
}

func TestDialPortRejectsPortZero(t *testing.T) {
	_, err := DialPort(context.Background(), &pipeDialer{}, 0)
	require.Error(t, err)
}

func TestDialPortRejectsUnknownStatus(t *testing.T) {
	var gotPort uint16
	dialer := &pipeDialer{serve: answerPortForward(7, &gotPort)}
	_, err := DialPort(context.Background(), dialer, 9222)
	require.ErrorIs(t, err, ErrPortForwarderUnavailable, "an unknown status is a forwarder error, not a refused port")
	require.ErrorIs(t, err, portforward.ErrUnknownStatus)
}

// Cancelling the request while the agent has not answered closes the connection
// instead of waiting out the handshake deadline.
func TestDialPortCancelledDuringHandshake(t *testing.T) {
	agentClosed := make(chan struct{})
	dialer := &pipeDialer{serve: func(agent net.Conn) {
		defer close(agentClosed)
		var port uint16
		_ = binary.Read(agent, binary.BigEndian, &port)
		// Never answer; a read only returns once the host closes the connection.
		_, _ = agent.Read(make([]byte, 1))
	}}
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(50*time.Millisecond, cancel)
	start := time.Now()
	_, err := DialPort(ctx, dialer, 9222)
	require.ErrorIs(t, err, context.Canceled)
	require.Less(t, time.Since(start), 5*time.Second)
	select {
	case <-agentClosed:
	case <-time.After(5 * time.Second):
		t.Fatal("the cancelled handshake left the connection open")
	}
}
