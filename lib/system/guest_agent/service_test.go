package main

import (
	"context"
	"io"
	"net"
	"os"
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
