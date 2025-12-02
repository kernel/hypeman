package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"sync"
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

	log.Printf("[exec-agent] exec: command=%v tty=%v cwd=%s timeout=%d",
		command, start.Tty, start.Cwd, start.TimeoutSeconds)

	// Create context with timeout if specified
	ctx := context.Background()
	if start.TimeoutSeconds > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(start.TimeoutSeconds)*time.Second)
		defer cancel()
	}

	if start.Tty {
		return s.executeTTY(ctx, stream, start)
	}
	return s.executeNoTTY(ctx, stream, start)
}

// executeNoTTY executes command without TTY
func (s *execServer) executeNoTTY(ctx context.Context, stream pb.ExecService_ExecServer, start *pb.ExecStart) error {
	// Run command directly - exec-agent is already running in container namespace
	if len(start.Command) == 0 {
		return fmt.Errorf("empty command")
	}
	
	cmd := exec.CommandContext(ctx, start.Command[0], start.Command[1:]...)
	
	// Set up environment
	cmd.Env = s.buildEnv(start.Env)
	
	// Set up working directory
	if start.Cwd != "" {
		cmd.Dir = start.Cwd
	}

	stdin, _ := cmd.StdinPipe()
	stdout, _ := cmd.StdoutPipe()
	stderr, _ := cmd.StderrPipe()

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start command: %w", err)
	}

	// Mutex to protect concurrent stream.Send calls (gRPC streams are not thread-safe)
	var sendMu sync.Mutex

	// Use WaitGroup to ensure all output is read before sending
	var wg sync.WaitGroup
	var stdoutData, stderrData []byte

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

	// Read all stdout/stderr BEFORE calling Wait() - Wait() closes the pipes!
	wg.Add(1)
	go func() {
		defer wg.Done()
		data, _ := io.ReadAll(stdout)
		stdoutData = data
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		data, _ := io.ReadAll(stderr)
		stderrData = data
	}()

	// Wait for all reads to complete FIRST (before Wait closes pipes)
	wg.Wait()
	
	// Now safe to call Wait - pipes are fully drained
	waitErr := cmd.Wait()

	// Now stream output in chunks (streaming compatible)
	const chunkSize = 32 * 1024
	for i := 0; i < len(stdoutData); i += chunkSize {
		end := i + chunkSize
		if end > len(stdoutData) {
			end = len(stdoutData)
		}
		sendMu.Lock()
		stream.Send(&pb.ExecResponse{
			Response: &pb.ExecResponse_Stdout{Stdout: stdoutData[i:end]},
		})
		sendMu.Unlock()
	}
	for i := 0; i < len(stderrData); i += chunkSize {
		end := i + chunkSize
		if end > len(stderrData) {
			end = len(stderrData)
		}
		sendMu.Lock()
		stream.Send(&pb.ExecResponse{
			Response: &pb.ExecResponse_Stderr{Stderr: stderrData[i:end]},
		})
		sendMu.Unlock()
	}

	exitCode := int32(0)
	if cmd.ProcessState != nil {
		exitCode = int32(cmd.ProcessState.ExitCode())
	} else if waitErr != nil {
		// If killed by timeout, exit with 124 (GNU timeout convention)
		exitCode = 124
	}

	log.Printf("[exec-agent] command finished with exit code: %d", exitCode)

	// Send exit code
	return stream.Send(&pb.ExecResponse{
		Response: &pb.ExecResponse_ExitCode{ExitCode: exitCode},
	})
}

// executeTTY executes command with TTY
func (s *execServer) executeTTY(ctx context.Context, stream pb.ExecService_ExecServer, start *pb.ExecStart) error {
	// Run command directly with PTY - exec-agent is already running in container namespace
	// This ensures PTY and shell are in the same namespace, fixing Ctrl+C signal handling
	if len(start.Command) == 0 {
		return fmt.Errorf("empty command")
	}
	
	cmd := exec.CommandContext(ctx, start.Command[0], start.Command[1:]...)
	
	// Set up environment
	cmd.Env = s.buildEnv(start.Env)
	
	// Set up working directory
	if start.Cwd != "" {
		cmd.Dir = start.Cwd
	}
	
	// Start with PTY
	ptmx, err := pty.Start(cmd)
	if err != nil {
		return fmt.Errorf("start pty: %w", err)
	}
	defer ptmx.Close()

	// Mutex to protect concurrent stream.Send calls (gRPC streams are not thread-safe)
	var sendMu sync.Mutex

	// Use WaitGroup to ensure all output is sent before exit code
	var wg sync.WaitGroup

	// Handle stdin in background
	go func() {
		for {
			req, err := stream.Recv()
			if err != nil {
				return
			}

			if data := req.GetStdin(); data != nil {
				ptmx.Write(data)
			}
		}
	}()

	// Stream output
	wg.Add(1)
	go func() {
		defer wg.Done()
		buf := make([]byte, 32 * 1024)
		for {
			n, err := ptmx.Read(buf)
			if n > 0 {
				sendMu.Lock()
				stream.Send(&pb.ExecResponse{
					Response: &pb.ExecResponse_Stdout{Stdout: buf[:n]},
				})
				sendMu.Unlock()
			}
			if err != nil {
				return
			}
		}
	}()

	// Wait for command or context cancellation
	waitErr := cmd.Wait()
	
	// Wait for all output to be sent
	wg.Wait()

	exitCode := int32(0)
	if cmd.ProcessState != nil {
		exitCode = int32(cmd.ProcessState.ExitCode())
	} else if waitErr != nil {
		// If killed by timeout, exit with 124 (GNU timeout convention)
		exitCode = 124
	}

	log.Printf("[exec-agent] TTY command finished with exit code: %d", exitCode)

	// Send exit code
	return stream.Send(&pb.ExecResponse{
		Response: &pb.ExecResponse_ExitCode{ExitCode: exitCode},
	})
}

// buildEnv constructs environment variables by merging provided env with defaults
func (s *execServer) buildEnv(envMap map[string]string) []string {
	// Start with current environment as base
	env := os.Environ()
	
	// Merge in provided environment variables
	for k, v := range envMap {
		env = append(env, fmt.Sprintf("%s=%s", k, v))
	}
	
	return env
}
