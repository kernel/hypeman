package firecracker

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/kernel/hypeman/lib/hypervisor"
)

const (
	vsockDialTimeout      = 5 * time.Second
	vsockHandshakeTimeout = 5 * time.Second
)

func init() {
	hypervisor.RegisterVsockDialerFactory(hypervisor.TypeFirecracker, NewVsockDialer)
}

type VsockDialer struct {
	socketPath string
}

func NewVsockDialer(vsockSocket string, vsockCID int64) hypervisor.VsockDialer {
	return &VsockDialer{socketPath: vsockSocket}
}

func (d *VsockDialer) Key() string {
	return "firecracker:" + d.socketPath
}

func (d *VsockDialer) DialVsock(ctx context.Context, port int) (net.Conn, error) {
	dialTimeout := vsockDialTimeout
	if deadline, ok := ctx.Deadline(); ok {
		if remaining := time.Until(deadline); remaining < dialTimeout {
			dialTimeout = remaining
		}
	}

	dialer := net.Dialer{Timeout: dialTimeout}
	conn, err := dialer.DialContext(ctx, "unix", d.socketPath)
	if err != nil {
		return nil, fmt.Errorf("dial vsock socket %s: %w", d.socketPath, err)
	}

	if err := conn.SetDeadline(time.Now().Add(vsockHandshakeTimeout)); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("set handshake deadline: %w", err)
	}

	if _, err := conn.Write([]byte(fmt.Sprintf("CONNECT %d\n", port))); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("send vsock handshake: %w", err)
	}

	reader := bufio.NewReader(conn)
	response, err := reader.ReadString('\n')
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("read vsock handshake response (is exec-agent running in guest?): %w", err)
	}

	if err := conn.SetDeadline(time.Time{}); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("clear handshake deadline: %w", err)
	}

	response = strings.TrimSpace(response)
	if !strings.HasPrefix(response, "OK ") {
		_ = conn.Close()
		return nil, fmt.Errorf("vsock handshake failed: %s", response)
	}

	return &bufferedConn{Conn: conn, reader: reader}, nil
}

type bufferedConn struct {
	net.Conn
	reader *bufio.Reader
}

func (c *bufferedConn) Read(p []byte) (int, error) {
	return c.reader.Read(p)
}
