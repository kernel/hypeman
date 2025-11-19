package main

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"time"

	"github.com/creack/pty"
	"github.com/mdlayher/vsock"
	pb "github.com/onkernel/hypeman/lib/exec"
	"google.golang.org/grpc"
)

// execServer implements the gRPC ExecService
type execServer struct {
	pb.UnimplementedExecServiceServer
}

func main() {
	// Listen on vsock port 2222 with retries
	var l *vsock.Listener
	var err error
	
	for i := 0; i < 10; i++ {
		l, err = vsock.Listen(2222, nil)
		if err == nil {
			break
		}
		log.Printf("[exec-agent] vsock listen attempt %d/10 failed: %v (retrying in 1s)", i+1, err)
		time.Sleep(1 * time.Second)
	}
	
	if err != nil {
		log.Fatalf("[exec-agent] failed to listen on vsock port 2222 after retries: %v", err)
	}
	defer l.Close()

	log.Println("[exec-agent] listening on vsock port 2222")

	// Create gRPC server
	grpcServer := grpc.NewServer()
	pb.RegisterExecServiceServer(grpcServer, &execServer{})

	// Serve gRPC over vsock
	if err := grpcServer.Serve(l); err != nil {
		log.Fatalf("[exec-agent] gRPC server failed: %v", err)
	}
}

// Exec handles command execution with bidirectional streaming
func (s *execServer) Exec(stream pb.ExecService_ExecServer) error {
	log.Printf("[exec-agent] new exec stream")

	// Receive start request
	req, err := stream.Recv()
	if err != nil {
		return fmt.Errorf("receive start request: %w", err)
	}

	start := req.GetStart()
	if start == nil {
		return fmt.Errorf("first message must be ExecStart")
	}

	command := start.Command
	if len(command) == 0 {
		command = []string{"/bin/sh"}
	}

	log.Printf("[exec-agent] exec: command=%v tty=%v", command, start.Tty)

	if start.Tty {
		return s.executeTTY(stream, command)
	}
	return s.executeNoTTY(stream, command)
}

// executeNoTTY executes command without TTY
func (s *execServer) executeNoTTY(stream pb.ExecService_ExecServer, command []string) error {
	// Chroot into container
	cmd := exec.Command("chroot", append([]string{"/overlay/newroot"}, command...)...)
	cmd.Env = os.Environ()

	stdin, _ := cmd.StdinPipe()
	stdout, _ := cmd.StdoutPipe()
	stderr, _ := cmd.StderrPipe()

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start command: %w", err)
	}

	// Handle stdin in background
	go func() {
		defer stdin.Close()
		for {
			req, err := stream.Recv()
			if err != nil {
				return
			}
			if data := req.GetStdin(); data != nil {
				stdin.Write(data)
			}
		}
	}()

	// Stream stdout
	go func() {
		buf := make([]byte, 32 * 1024)
		for {
			n, err := stdout.Read(buf)
			if n > 0 {
				stream.Send(&pb.ExecResponse{
					Response: &pb.ExecResponse_Stdout{Stdout: buf[:n]},
				})
			}
			if err != nil {
				return
			}
		}
	}()

	// Stream stderr
	go func() {
		buf := make([]byte, 32 * 1024)
		for {
			n, err := stderr.Read(buf)
			if n > 0 {
				stream.Send(&pb.ExecResponse{
					Response: &pb.ExecResponse_Stderr{Stderr: buf[:n]},
				})
			}
			if err != nil {
				return
			}
		}
	}()

	// Wait for command to finish
	cmd.Wait()

	exitCode := int32(0)
	if cmd.ProcessState != nil {
		exitCode = int32(cmd.ProcessState.ExitCode())
	}

	log.Printf("[exec-agent] command finished with exit code: %d", exitCode)

	// Send exit code
	return stream.Send(&pb.ExecResponse{
		Response: &pb.ExecResponse_ExitCode{ExitCode: exitCode},
	})
}

// executeTTY executes command with TTY
func (s *execServer) executeTTY(stream pb.ExecService_ExecServer, command []string) error {
	// Chroot into container
	cmd := exec.Command("chroot", append([]string{"/overlay/newroot"}, command...)...)
	cmd.Env = os.Environ()

	// Start with PTY
	ptmx, err := pty.Start(cmd)
	if err != nil {
		return fmt.Errorf("start pty: %w", err)
	}
	defer ptmx.Close()

	// Handle input (stdin + resize)
	go func() {
		for {
			req, err := stream.Recv()
			if err != nil {
				return
			}

			if data := req.GetStdin(); data != nil {
				ptmx.Write(data)
			} else if resize := req.GetResize(); resize != nil {
				pty.Setsize(ptmx, &pty.Winsize{
					Rows: uint16(resize.Rows),
					Cols: uint16(resize.Cols),
				})
			}
		}
	}()

	// Stream output
	go func() {
		buf := make([]byte, 32 * 1024)
		for {
			n, err := ptmx.Read(buf)
			if n > 0 {
				stream.Send(&pb.ExecResponse{
					Response: &pb.ExecResponse_Stdout{Stdout: buf[:n]},
				})
			}
			if err != nil {
				return
			}
		}
	}()

	// Wait for command
	cmd.Wait()

	exitCode := int32(0)
	if cmd.ProcessState != nil {
		exitCode = int32(cmd.ProcessState.ExitCode())
	}

	log.Printf("[exec-agent] TTY command finished with exit code: %d", exitCode)

	// Send exit code
	return stream.Send(&pb.ExecResponse{
		Response: &pb.ExecResponse_ExitCode{ExitCode: exitCode},
	})
}
