package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/user"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"
	"github.com/mdlayher/vsock"
	pb "github.com/onkernel/hypeman/lib/exec"
	"golang.org/x/sys/unix"
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

	log.Printf("[exec-agent] exec: command=%v tty=%v user=%s uid=%d cwd=%s timeout=%d",
		command, start.Tty, start.User, start.Uid, start.Cwd, start.TimeoutSeconds)

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
	
	// Set up user/uid credentials
	if cred, err := s.buildCredentials(start); err == nil && cred != nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{
			Credential: cred,
		}
	} else if err != nil {
		log.Printf("[exec-agent] warning: failed to set credentials: %v", err)
	}

	stdin, _ := cmd.StdinPipe()
	stdout, _ := cmd.StdoutPipe()
	stderr, _ := cmd.StderrPipe()

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start command: %w", err)
	}

	// Use WaitGroup to ensure all output is sent before exit code
	var wg sync.WaitGroup

	// Handle stdin and signals in background
	go func() {
		defer stdin.Close()
		for {
			req, err := stream.Recv()
			if err != nil {
				return
			}
			if data := req.GetStdin(); data != nil {
				stdin.Write(data)
			} else if sig := req.GetSignal(); sig != nil {
				s.sendSignal(cmd, sig.Signal)
			}
		}
	}()

	// Stream stdout
	wg.Add(1)
	go func() {
		defer wg.Done()
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
	wg.Add(1)
	go func() {
		defer wg.Done()
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

	// Wait for command to finish or context cancellation
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
	
	// Temporarily disable credential handling to test if that's the Ctrl+C issue
	// TODO: Implement credential handling properly with PTY
	if start.User != "" || start.Uid != 0 {
		log.Printf("[exec-agent] warning: user/uid switching not yet supported in TTY mode")
	}
	
	// Start with PTY - let pty.Start() handle all session/controlling terminal setup
	ptmx, err := pty.Start(cmd)
	if err != nil {
		return fmt.Errorf("start pty: %w", err)
	}
	defer ptmx.Close()

	// Debug: Check PTY terminal attributes and process group info
	if termios, err := unix.IoctlGetTermios(int(ptmx.Fd()), unix.TCGETS); err == nil {
		log.Printf("[exec-agent] PTY termios: Lflag=%#x VINTR=%d VQUIT=%d VSUSP=%d ISIG=%v", 
			termios.Lflag, 
			termios.Cc[unix.VINTR], 
			termios.Cc[unix.VQUIT], 
			termios.Cc[unix.VSUSP],
			(termios.Lflag & unix.ISIG) != 0)
	} else {
		log.Printf("[exec-agent] warning: could not get PTY termios: %v", err)
	}
	
	// Debug: Check process and controlling terminal info
	if cmd.Process != nil {
		pid := cmd.Process.Pid
		pgid, _ := unix.Getpgid(pid)
		sid, _ := unix.Getsid(pid)
		foreground, _ := unix.IoctlGetInt(int(ptmx.Fd()), unix.TIOCGPGRP)
		log.Printf("[exec-agent] Process info: PID=%d PGID=%d SID=%d PTY_foreground_pgid=%d", pid, pgid, sid, foreground)
		
		// Explicitly set this process group as the PTY foreground
		if err := unix.IoctlSetInt(int(ptmx.Fd()), unix.TIOCSPGRP, pgid); err != nil {
			log.Printf("[exec-agent] warning: failed to set foreground pgid: %v", err)
		} else {
			log.Printf("[exec-agent] set PTY foreground pgid to %d", pgid)
		}
	}

	// Use WaitGroup to ensure all output is sent before exit code
	var wg sync.WaitGroup

	// Handle input (stdin + resize + signals)
	go func() {
		for {
			req, err := stream.Recv()
			if err != nil {
				return
			}

			if data := req.GetStdin(); data != nil {
				// Debug: Log control characters and check foreground pgid
				for _, b := range data {
					if b < 32 {
						foreground, _ := unix.IoctlGetInt(int(ptmx.Fd()), unix.TIOCGPGRP)
						log.Printf("[exec-agent] received control char: 0x%02x (%d), PTY_foreground_pgid=%d", b, b, foreground)
					}
				}
				ptmx.Write(data)
			} else if resize := req.GetResize(); resize != nil {
				pty.Setsize(ptmx, &pty.Winsize{
					Rows: uint16(resize.Rows),
					Cols: uint16(resize.Cols),
				})
			} else if sig := req.GetSignal(); sig != nil {
				s.sendSignal(cmd, sig.Signal)
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
				stream.Send(&pb.ExecResponse{
					Response: &pb.ExecResponse_Stdout{Stdout: buf[:n]},
				})
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

// buildCredentials creates syscall.Credential from user/uid fields
func (s *execServer) buildCredentials(start *pb.ExecStart) (*syscall.Credential, error) {
	// If neither user nor uid is specified, return nil (run as current user)
	if start.User == "" && start.Uid == 0 {
		return nil, nil
	}
	
	var uid, gid uint32
	
	// UID takes precedence over username
	if start.Uid > 0 {
		uid = uint32(start.Uid)
		// Try to get GID from passwd, fallback to same as UID
		if u, err := user.LookupId(strconv.Itoa(int(start.Uid))); err == nil {
			if g, err := strconv.Atoi(u.Gid); err == nil {
				gid = uint32(g)
			} else {
				gid = uid
			}
		} else {
			gid = uid
		}
	} else if start.User != "" {
		// Look up user by name
		u, err := user.Lookup(start.User)
		if err != nil {
			return nil, fmt.Errorf("lookup user %s: %w", start.User, err)
		}
		
		uidInt, err := strconv.Atoi(u.Uid)
		if err != nil {
			return nil, fmt.Errorf("parse uid: %w", err)
		}
		uid = uint32(uidInt)
		
		gidInt, err := strconv.Atoi(u.Gid)
		if err != nil {
			return nil, fmt.Errorf("parse gid: %w", err)
		}
		gid = uint32(gidInt)
	}
	
	return &syscall.Credential{
		Uid: uid,
		Gid: gid,
	}, nil
}

// sendSignal sends a Unix signal to the process
func (s *execServer) sendSignal(cmd *exec.Cmd, sig pb.SignalType) {
	if cmd == nil || cmd.Process == nil {
		log.Printf("[exec-agent] cannot send signal: process not started")
		return
	}
	
	var unixSig syscall.Signal
	switch sig {
	case pb.SignalType_SIGHUP:
		unixSig = syscall.SIGHUP
	case pb.SignalType_SIGINT:
		unixSig = syscall.SIGINT
	case pb.SignalType_SIGQUIT:
		unixSig = syscall.SIGQUIT
	case pb.SignalType_SIGKILL:
		unixSig = syscall.SIGKILL
	case pb.SignalType_SIGTERM:
		unixSig = syscall.SIGTERM
	case pb.SignalType_SIGSTOP:
		unixSig = syscall.SIGSTOP
	case pb.SignalType_SIGCONT:
		unixSig = syscall.SIGCONT
	default:
		log.Printf("[exec-agent] unknown signal type: %v", sig)
		return
	}
	
	log.Printf("[exec-agent] sending signal %v to process %d", sig, cmd.Process.Pid)
	if err := cmd.Process.Signal(unixSig); err != nil {
		log.Printf("[exec-agent] failed to send signal: %v", err)
	}
}
