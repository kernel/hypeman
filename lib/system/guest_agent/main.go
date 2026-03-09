package main

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"time"

	pb "github.com/kernel/hypeman/lib/guest"
	"github.com/mdlayher/vsock"
	"google.golang.org/grpc"
)

const (
	readySentinelPrefix  = "HYPEMAN-AGENT-READY"
	defaultReadyFilePath = "/run/hypeman/guest-agent-ready"
	readyFDEnv           = "HYPEMAN_AGENT_READY_FD"
)

// guestServer implements the gRPC GuestService
type guestServer struct {
	pb.UnimplementedGuestServiceServer
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
		log.Printf("[guest-agent] vsock listen attempt %d/10 failed: %v (retrying in 1s)", i+1, err)
		time.Sleep(1 * time.Second)
	}

	if err != nil {
		log.Fatalf("[guest-agent] failed to listen on vsock port 2222 after retries: %v", err)
	}
	defer l.Close()

	log.Println("[guest-agent] listening on vsock port 2222")
	log.Printf("[guest-agent] %s ts=%s", readySentinelPrefix, time.Now().UTC().Format(time.RFC3339Nano))
	if err := signalReadyFD(); err != nil {
		log.Printf("[guest-agent] warning: failed to signal readiness fd: %v", err)
	}
	if err := writeReadyFile(); err != nil {
		log.Printf("[guest-agent] warning: failed to write readiness file: %v", err)
	}

	// Create gRPC server
	grpcServer := grpc.NewServer()
	pb.RegisterGuestServiceServer(grpcServer, &guestServer{})

	// Serve gRPC over vsock
	if err := grpcServer.Serve(l); err != nil {
		log.Fatalf("[guest-agent] gRPC server failed: %v", err)
	}
}

func writeReadyFile() error {
	path := os.Getenv("HYPEMAN_AGENT_READY_FILE")
	if path == "" {
		path = defaultReadyFilePath
	}
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(time.Now().UTC().Format(time.RFC3339Nano)+"\n"), 0644)
}

func signalReadyFD() error {
	rawFD := os.Getenv(readyFDEnv)
	if rawFD == "" {
		return nil
	}

	fd, err := strconv.Atoi(rawFD)
	if err != nil {
		return fmt.Errorf("parse %s: %w", readyFDEnv, err)
	}
	if fd < 0 {
		return fmt.Errorf("invalid %s=%d", readyFDEnv, fd)
	}

	f := os.NewFile(uintptr(fd), "guest-agent-ready-fd")
	if f == nil {
		return fmt.Errorf("open readiness fd %d", fd)
	}
	defer f.Close()

	if _, err := f.Write([]byte{1}); err != nil {
		return fmt.Errorf("write readiness byte: %w", err)
	}
	return nil
}
