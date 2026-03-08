package main

import (
	"log"
	"os"
	"path/filepath"
	"time"

	pb "github.com/kernel/hypeman/lib/guest"
	"github.com/mdlayher/vsock"
	"google.golang.org/grpc"
)

const (
	readySentinelPrefix  = "HYPEMAN-AGENT-READY"
	defaultReadyFilePath = "/run/hypeman/guest-agent-ready"
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
