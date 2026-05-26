package guest

import (
	"context"
	"errors"
	"io"
	"net"
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
