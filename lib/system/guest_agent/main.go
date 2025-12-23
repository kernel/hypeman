package main

import (
	"log"
	"time"

	"github.com/mdlayher/vsock"
	pb "github.com/onkernel/hypeman/lib/guest"
	"google.golang.org/grpc"
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

	// Create gRPC server
	grpcServer := grpc.NewServer()
	pb.RegisterGuestServiceServer(grpcServer, &guestServer{})

	// Serve gRPC over vsock
	if err := grpcServer.Serve(l); err != nil {
		log.Fatalf("[guest-agent] gRPC server failed: %v", err)
	}
}
