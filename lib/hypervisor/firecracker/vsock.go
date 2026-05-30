package firecracker

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/kernel/hypeman/lib/hypervisor"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
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

func (d *VsockDialer) DialVsock(ctx context.Context, port int) (_ net.Conn, retErr error) {
	ctx, span := hypervisor.StartImplementationSpan(ctx, hypervisor.TypeFirecracker, "hypervisor.vsock.dial",
		attribute.Int("vsock.port", port),
	)
	defer func() { hypervisor.FinishTraceSpan(span, retErr) }()

	dialTimeout := vsockDialTimeout
	if deadline, ok := ctx.Deadline(); ok {
		if remaining := time.Until(deadline); remaining < dialTimeout {
			dialTimeout = remaining
		}
	}
	span.SetAttributes(attribute.Int64("dial_timeout_ms", dialTimeout.Milliseconds()))

	dialer := net.Dialer{Timeout: dialTimeout}
	stepCtx, stepSpan := otel.Tracer("hypeman/hypervisor/firecracker").Start(ctx, "hypervisor.vsock.unix_dial")
	conn, err := dialer.DialContext(stepCtx, "unix", d.socketPath)
	hypervisor.FinishTraceSpan(stepSpan, err)
	if err != nil {
		retErr = fmt.Errorf("dial vsock socket %s: %w", d.socketPath, err)
		return nil, retErr
	}

	if err := conn.SetDeadline(time.Now().Add(vsockHandshakeTimeout)); err != nil {
		_ = conn.Close()
		retErr = fmt.Errorf("set handshake deadline: %w", err)
		return nil, retErr
	}

	_, stepSpan = otel.Tracer("hypeman/hypervisor/firecracker").Start(ctx, "hypervisor.vsock.write_connect")
	if _, err := conn.Write([]byte(fmt.Sprintf("CONNECT %d\n", port))); err != nil {
		hypervisor.FinishTraceSpan(stepSpan, err)
		_ = conn.Close()
		retErr = fmt.Errorf("send vsock handshake: %w", err)
		return nil, retErr
	}
	hypervisor.FinishTraceSpan(stepSpan, nil)

	reader := bufio.NewReader(conn)
	_, stepSpan = otel.Tracer("hypeman/hypervisor/firecracker").Start(ctx, "hypervisor.vsock.read_ok")
	response, err := reader.ReadString('\n')
	if err != nil {
		hypervisor.FinishTraceSpan(stepSpan, err)
		_ = conn.Close()
		retErr = fmt.Errorf("read vsock handshake response (is exec-agent running in guest?): %w", err)
		return nil, retErr
	}
	hypervisor.FinishTraceSpan(stepSpan, nil)

	if err := conn.SetDeadline(time.Time{}); err != nil {
		_ = conn.Close()
		retErr = fmt.Errorf("clear handshake deadline: %w", err)
		return nil, retErr
	}

	response = strings.TrimSpace(response)
	if !strings.HasPrefix(response, "OK ") {
		_ = conn.Close()
		retErr = fmt.Errorf("vsock handshake failed: %s", response)
		return nil, retErr
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
