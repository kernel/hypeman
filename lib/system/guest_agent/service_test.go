package main

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	pb "github.com/kernel/hypeman/lib/guest"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
)

// Exercise the shared RPC contract without starting a VM or listening on vsock.
func testGuestClient(t *testing.T) pb.GuestServiceClient {
	t.Helper()
	listener := bufconn.Listen(64 << 10)
	server := grpc.NewServer()
	pb.RegisterGuestServiceServer(server, &guestServer{})
	go server.Serve(listener)
	t.Cleanup(server.Stop)
	t.Cleanup(func() { listener.Close() })
	conn, err := grpc.NewClient("passthrough:///guest-test", grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) {
		return listener.Dial()
	}))
	require.NoError(t, err)
	t.Cleanup(func() { conn.Close() })
	return pb.NewGuestServiceClient(conn)
}

func TestGuestServiceExecRoundTrip(t *testing.T) {
	client := testGuestClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stream, err := client.Exec(ctx)
	require.NoError(t, err)
	require.NoError(t, stream.Send(&pb.ExecRequest{Request: &pb.ExecRequest_Start{Start: &pb.ExecStart{
		Command: []string{"/bin/sh", "-c", "printf '%s' \"$HYPEMAN_EXEC_TEST\"; printf 'stderr' >&2; exit 7"},
		Env:     map[string]string{"HYPEMAN_EXEC_TEST": "stdout"},
	}}}))
	require.NoError(t, stream.CloseSend())
	var stdout, stderr strings.Builder
	var exitCode *int32
	for {
		response, err := stream.Recv()
		if err == io.EOF {
			break
		}
		require.NoError(t, err)
		switch value := response.Response.(type) {
		case *pb.ExecResponse_Stdout:
			stdout.Write(value.Stdout)
		case *pb.ExecResponse_Stderr:
			stderr.Write(value.Stderr)
		case *pb.ExecResponse_ExitCode:
			code := value.ExitCode
			exitCode = &code
		}
	}
	require.Equal(t, "stdout", stdout.String())
	require.Equal(t, "stderr", stderr.String())
	require.NotNil(t, exitCode)
	require.Equal(t, int32(7), *exitCode)
}

func TestGuestServiceExecStreamsBeforeExit(t *testing.T) {
	client := testGuestClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stream, err := client.Exec(ctx)
	require.NoError(t, err)
	require.NoError(t, stream.Send(&pb.ExecRequest{Request: &pb.ExecRequest_Start{Start: &pb.ExecStart{
		Command: []string{"/bin/sh", "-c", "printf ready; read input; printf '%s' \"$input\""},
	}}}))
	first, err := stream.Recv()
	require.NoError(t, err, "output must arrive while the command is still waiting for stdin")
	require.Equal(t, "ready", string(first.GetStdout()))
	require.NoError(t, stream.Send(&pb.ExecRequest{Request: &pb.ExecRequest_Stdin{Stdin: []byte("finish\n")}}))
	require.NoError(t, stream.CloseSend())
	var output strings.Builder
	exited := false
	for {
		response, err := stream.Recv()
		if err == io.EOF {
			break
		}
		require.NoError(t, err)
		output.Write(response.GetStdout())
		if code, ok := response.Response.(*pb.ExecResponse_ExitCode); ok {
			require.Equal(t, int32(0), code.ExitCode)
			exited = true
		}
	}
	require.True(t, exited)
	require.Equal(t, "finish", output.String())
}

func TestGuestServiceExecCancellation(t *testing.T) {
	client := testGuestClient(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pidPath := filepath.Join(t.TempDir(), "pid")
	stream, err := client.Exec(ctx)
	require.NoError(t, err)
	require.NoError(t, stream.Send(&pb.ExecRequest{Request: &pb.ExecRequest_Start{Start: &pb.ExecStart{
		Command: []string{"/bin/sh", "-c", "printf '%s' \"$$\" > \"$HYPEMAN_PID_FILE\"; exec /bin/sleep 30"},
		Env:     map[string]string{"HYPEMAN_PID_FILE": pidPath},
	}}}))
	require.NoError(t, stream.CloseSend())
	var pid int
	require.Eventually(t, func() bool {
		data, err := os.ReadFile(pidPath)
		if err != nil {
			return false
		}
		pid, err = strconv.Atoi(string(data))
		return err == nil && pid > 0
	}, 5*time.Second, 10*time.Millisecond)
	defer syscall.Kill(pid, syscall.SIGKILL)
	cancel()
	require.Eventually(t, func() bool {
		return syscall.Kill(pid, 0) == syscall.ESRCH
	}, 5*time.Second, 10*time.Millisecond, "disconnect must cancel the command, not leave it running")
}

func TestGuestServiceExecCancellationKillsDescendants(t *testing.T) {
	for _, tty := range []bool{false, true} {
		t.Run(map[bool]string{false: "pipes", true: "tty"}[tty], func(t *testing.T) {
			client := testGuestClient(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			pidPath := filepath.Join(t.TempDir(), "pid")
			stream, err := client.Exec(ctx)
			require.NoError(t, err)
			require.NoError(t, stream.Send(&pb.ExecRequest{Request: &pb.ExecRequest_Start{Start: &pb.ExecStart{
				Command: []string{"/bin/sh", "-c", "sleep 30 & printf '%s' \"$!\" > \"$HYPEMAN_PID_FILE\"; wait"},
				Env:     map[string]string{"HYPEMAN_PID_FILE": pidPath},
				Tty:     tty,
			}}}))
			require.NoError(t, stream.CloseSend())
			var pid int
			require.Eventually(t, func() bool {
				data, err := os.ReadFile(pidPath)
				if err != nil {
					return false
				}
				pid, err = strconv.Atoi(string(data))
				return err == nil && pid > 0
			}, 5*time.Second, 10*time.Millisecond)
			defer syscall.Kill(pid, syscall.SIGKILL)
			cancel()
			require.Eventually(t, func() bool {
				return processGone(pid)
			}, 5*time.Second, 10*time.Millisecond, "cancellation must reach descendants, not only the shell")
		})
	}
}

// processGone reports whether pid no longer runs. A zombie counts as gone.
func processGone(pid int) bool {
	if errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) {
		return true
	}
	out, err := exec.Command("ps", "-o", "stat=", "-p", strconv.Itoa(pid)).Output()
	return err != nil || strings.HasPrefix(strings.TrimSpace(string(out)), "Z")
}

// stalledExecStream models a client that stopped reading: Send blocks until release closes.
// With stallExitOnly, only the final exit-code send blocks.
type stalledExecStream struct {
	grpc.ServerStream
	ctx           context.Context
	start         *pb.ExecRequest
	release       chan struct{}
	stallExitOnly bool
}

func (s *stalledExecStream) Context() context.Context { return s.ctx }
func (s *stalledExecStream) Recv() (*pb.ExecRequest, error) {
	if s.start != nil {
		req := s.start
		s.start = nil
		return req, nil
	}
	<-s.ctx.Done()
	return nil, s.ctx.Err()
}
func (s *stalledExecStream) Send(resp *pb.ExecResponse) error {
	if _, exit := resp.Response.(*pb.ExecResponse_ExitCode); s.stallExitOnly && !exit {
		return nil
	}
	<-s.release
	return nil
}

// runStalledExec runs a timed-out command against a client that stopped reading and
// requires the handler to return the bounded-drain error instead of hanging.
func runStalledExec(t *testing.T, command string, exitOnly bool) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	stream := &stalledExecStream{ctx: ctx, release: release, stallExitOnly: exitOnly, start: &pb.ExecRequest{Request: &pb.ExecRequest_Start{Start: &pb.ExecStart{
		Command:        []string{"/bin/sh", "-c", command},
		TimeoutSeconds: 1,
	}}}}
	done := make(chan error, 1)
	go func() { done <- (&guestServer{drainGrace: 100 * time.Millisecond}).Exec(stream) }()
	select {
	case err := <-done:
		require.ErrorContains(t, err, "did not finish")
	case <-time.After(10 * time.Second):
		t.Fatal("exec stayed blocked behind a stalled client after its timeout")
	}
}

func TestGuestServiceExecTimeoutFinishesWhenClientStopsReading(t *testing.T) {
	runStalledExec(t, "yes", false)
}

func TestGuestServiceExecTimeoutBoundsExitCodeSend(t *testing.T) {
	runStalledExec(t, "echo started; sleep 30", true)
}

func TestGuestServiceExecTimeoutKillsDescendantAfterShellExits(t *testing.T) {
	for _, tty := range []bool{false, true} {
		t.Run(map[bool]string{false: "pipes", true: "tty"}[tty], func(t *testing.T) {
			client := testGuestClient(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			pidPath := filepath.Join(t.TempDir(), "pid")
			stream, err := client.Exec(ctx)
			require.NoError(t, err)
			require.NoError(t, stream.Send(&pb.ExecRequest{Request: &pb.ExecRequest_Start{Start: &pb.ExecStart{
				Command:        []string{"/bin/sh", "-c", "sleep 30 & printf '%s' \"$!\" > \"$HYPEMAN_PID_FILE\"; exit 0"},
				Env:            map[string]string{"HYPEMAN_PID_FILE": pidPath},
				Tty:            tty,
				TimeoutSeconds: 1,
			}}}))
			require.NoError(t, stream.CloseSend())
			var pid int
			require.Eventually(t, func() bool {
				data, err := os.ReadFile(pidPath)
				if err != nil {
					return false
				}
				pid, err = strconv.Atoi(string(data))
				return err == nil && pid > 0
			}, 5*time.Second, 10*time.Millisecond)
			defer syscall.Kill(pid, syscall.SIGKILL)
			finished := make(chan error, 1)
			go func() {
				for {
					if _, err := stream.Recv(); err != nil {
						finished <- err
						return
					}
				}
			}()
			select {
			case <-finished:
			case <-time.After(10 * time.Second):
				t.Fatal("RPC stayed open while a descendant held the command's output")
			}
			require.True(t, processGone(pid), "timeout must kill the descendant even after the shell exited")
		})
	}
}

// runExec drives one exec to completion and returns its stdout and exit code.
func runExec(t *testing.T, start *pb.ExecStart) (string, int32) {
	t.Helper()
	client := testGuestClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	stream, err := client.Exec(ctx)
	require.NoError(t, err)
	require.NoError(t, stream.Send(&pb.ExecRequest{Request: &pb.ExecRequest_Start{Start: start}}))
	require.NoError(t, stream.CloseSend())
	var stdout strings.Builder
	exit := int32(-1)
	for {
		resp, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		require.NoError(t, err)
		stdout.Write(resp.GetStdout())
		if code, ok := resp.Response.(*pb.ExecResponse_ExitCode); ok {
			exit = code.ExitCode
		}
	}
	return stdout.String(), exit
}

func TestGuestServiceExecExitZeroWhenDescendantHoldsOutput(t *testing.T) {
	stdout, exit := runExec(t, &pb.ExecStart{Command: []string{"/bin/sh", "-c", "sleep 5 & echo hi"}})
	require.Equal(t, "hi\n", stdout)
	require.Equal(t, int32(0), exit, "a command that exits 0 must report 0, not a stream error")
}

func TestGuestServiceExecTimeoutReports124(t *testing.T) {
	_, exit := runExec(t, &pb.ExecStart{Command: []string{"/bin/sleep", "30"}, TimeoutSeconds: 1})
	require.Equal(t, int32(124), exit)
}

func TestGuestServiceFileRoundTrip(t *testing.T) {
	client := testGuestClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	path := filepath.Join(t.TempDir(), "copied")
	payload := "shared guest service file payload"
	stream, err := client.CopyToGuest(ctx)
	require.NoError(t, err)
	require.NoError(t, stream.Send(&pb.CopyToGuestRequest{Request: &pb.CopyToGuestRequest_Start{Start: &pb.CopyToGuestStart{Path: path, Mode: 0600, Size: int64(len(payload))}}}))
	require.NoError(t, stream.Send(&pb.CopyToGuestRequest{Request: &pb.CopyToGuestRequest_Data{Data: []byte(payload)}}))
	require.NoError(t, stream.Send(&pb.CopyToGuestRequest{Request: &pb.CopyToGuestRequest_End{End: &pb.CopyToGuestEnd{}}}))
	result, err := stream.CloseAndRecv()
	require.NoError(t, err)
	require.True(t, result.Success, result.Error)
	require.Equal(t, int64(len(payload)), result.BytesWritten)
	info, err := client.StatPath(ctx, &pb.StatPathRequest{Path: path})
	require.NoError(t, err)
	require.True(t, info.Exists && info.IsFile)
	require.Equal(t, uint32(0600), info.Mode&0777)
	require.Equal(t, int64(len(payload)), info.Size)
	read, err := client.CopyFromGuest(ctx, &pb.CopyFromGuestRequest{Path: path})
	require.NoError(t, err)
	var content strings.Builder
	final := false
	for {
		response, err := read.Recv()
		if err == io.EOF {
			break
		}
		require.NoError(t, err)
		require.Nil(t, response.GetError())
		content.Write(response.GetData())
		if response.GetEnd() != nil && response.GetEnd().Final {
			final = true
		}
	}
	require.True(t, final)
	require.Equal(t, payload, content.String())
}
