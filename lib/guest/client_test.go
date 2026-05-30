package guest

import (
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kernel/hypeman/lib/hypervisor"
	"google.golang.org/grpc"
)

func TestExecCommandName(t *testing.T) {
	tests := []struct {
		name    string
		command []string
		want    string
	}{
		{
			name:    "empty command defaults to shell",
			command: nil,
			want:    "/bin/sh",
		},
		{
			name:    "empty binary defaults to shell",
			command: []string{""},
			want:    "/bin/sh",
		},
		{
			name:    "uses basename",
			command: []string{"/usr/bin/ip", "addr", "show"},
			want:    "ip",
		},
		{
			name:    "does not include arguments",
			command: []string{"/bin/bash", "-lc", "secret-bearing command"},
			want:    "bash",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := execCommandName(tt.command); got != tt.want {
				t.Fatalf("execCommandName(%v) = %q, want %q", tt.command, got, tt.want)
			}
		})
	}
}

func TestGuestExecRetryInterval(t *testing.T) {
	tests := []struct {
		name    string
		elapsed time.Duration
		want    time.Duration
	}{
		{
			name:    "fast path",
			elapsed: 500 * time.Millisecond,
			want:    guestExecFastRetryInterval,
		},
		{
			name:    "boundary",
			elapsed: guestExecFastRetryWindow,
			want:    guestExecSlowRetryInterval,
		},
		{
			name:    "slow path",
			elapsed: 3 * time.Second,
			want:    guestExecSlowRetryInterval,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := guestExecRetryInterval(tt.elapsed); got != tt.want {
				t.Fatalf("guestExecRetryInterval(%s) = %s, want %s", tt.elapsed, got, tt.want)
			}
		})
	}
}

func TestExecIntoInstanceRetriesWithFreshConnections(t *testing.T) {
	dialer := &delayedDialer{
		key:     "retry-fresh-connection-test",
		readyAt: time.Now().Add(100 * time.Millisecond),
	}

	start := time.Now()
	exit, err := ExecIntoInstance(context.Background(), dialer, ExecOptions{
		Command:      []string{"true"},
		WaitForAgent: 2 * time.Second,
	})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("ExecIntoInstance failed: %v", err)
	}
	if exit.Code != 0 {
		t.Fatalf("exit code = %d, want 0", exit.Code)
	}
	if attempts := dialer.attempts.Load(); attempts < 2 {
		t.Fatalf("dial attempts = %d, want retry", attempts)
	}
	if elapsed > 500*time.Millisecond {
		t.Fatalf("ExecIntoInstance took %s, want under 500ms", elapsed)
	}
}

func TestExecIntoInstanceNoWaitClosesRetryableConnection(t *testing.T) {
	dialer := &alwaysFailDialer{key: "no-wait-close-retryable-test"}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	_, err := ExecIntoInstance(ctx, dialer, ExecOptions{
		Command: []string{"true"},
	})
	if err == nil {
		t.Fatal("ExecIntoInstance succeeded unexpectedly")
	}

	connPool.RLock()
	_, ok := connPool.conns[dialer.Key()]
	connPool.RUnlock()
	if ok {
		t.Fatal("retryable no-wait exec error left connection in pool")
	}
}

func TestCloseConnClosesPooledConnection(t *testing.T) {
	dialer := &trackingDialer{
		key:   "close-conn-test",
		conns: make(chan *closeTrackingConn, 1),
	}

	conn, err := GetOrCreateConn(context.Background(), dialer)
	if err != nil {
		t.Fatalf("GetOrCreateConn failed: %v", err)
	}
	conn.Connect()

	tracked := waitForTrackedConn(t, dialer.conns)
	CloseConn(dialer.Key())

	select {
	case <-tracked.closed:
	case <-time.After(time.Second):
		t.Fatal("CloseConn did not close the underlying connection")
	}
}

func waitForTrackedConn(t *testing.T, conns <-chan *closeTrackingConn) *closeTrackingConn {
	t.Helper()

	select {
	case conn := <-conns:
		return conn
	case <-time.After(time.Second):
		t.Fatal("gRPC connection was not dialed")
		return nil
	}
}

type delayedDialer struct {
	key      string
	readyAt  time.Time
	attempts atomic.Int32
}

func (d *delayedDialer) Key() string { return d.key }

func (d *delayedDialer) DialVsock(ctx context.Context, port int) (net.Conn, error) {
	d.attempts.Add(1)
	if time.Now().Before(d.readyAt) {
		return nil, errors.New("not ready")
	}

	client, server := net.Pipe()
	go serveFakeGuest(server)
	return client, nil
}

var _ hypervisor.VsockDialer = (*delayedDialer)(nil)

type alwaysFailDialer struct {
	key string
}

func (d *alwaysFailDialer) Key() string { return d.key }

func (d *alwaysFailDialer) DialVsock(ctx context.Context, port int) (net.Conn, error) {
	return nil, errors.New("not ready")
}

var _ hypervisor.VsockDialer = (*alwaysFailDialer)(nil)

type trackingDialer struct {
	key   string
	conns chan *closeTrackingConn
}

func (d *trackingDialer) Key() string { return d.key }

func (d *trackingDialer) DialVsock(ctx context.Context, port int) (net.Conn, error) {
	client, server := net.Pipe()
	tracked := &closeTrackingConn{
		Conn:   client,
		closed: make(chan struct{}),
	}
	select {
	case d.conns <- tracked:
	default:
	}
	go serveFakeGuest(server)
	return tracked, nil
}

var _ hypervisor.VsockDialer = (*trackingDialer)(nil)

type closeTrackingConn struct {
	net.Conn
	closed chan struct{}
	once   sync.Once
}

func (c *closeTrackingConn) Close() error {
	c.once.Do(func() {
		close(c.closed)
	})
	return c.Conn.Close()
}

type fakeGuestServer struct {
	UnimplementedGuestServiceServer
}

func (s *fakeGuestServer) Exec(stream GuestService_ExecServer) error {
	if _, err := stream.Recv(); err != nil {
		return err
	}
	if err := stream.Send(&ExecResponse{Response: &ExecResponse_ExitCode{ExitCode: 0}}); err != nil {
		return err
	}
	for {
		if _, err := stream.Recv(); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
	}
}

func serveFakeGuest(conn net.Conn) {
	s := grpc.NewServer()
	RegisterGuestServiceServer(s, &fakeGuestServer{})
	_ = s.Serve(&singleConnListener{conn: conn})
}

type singleConnListener struct {
	conn net.Conn
	done atomic.Bool
}

func (l *singleConnListener) Accept() (net.Conn, error) {
	if l.done.Swap(true) {
		return nil, net.ErrClosed
	}
	return l.conn, nil
}

func (l *singleConnListener) Close() error { return nil }

func (l *singleConnListener) Addr() net.Addr { return dummyAddr{} }

type dummyAddr struct{}

func (dummyAddr) Network() string { return "pipe" }
func (dummyAddr) String() string  { return "pipe" }
