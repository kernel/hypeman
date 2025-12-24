package main

import (
	"context"
	"log"
	"time"

	"github.com/mdlayher/vsock"
	pb "github.com/onkernel/hypeman/lib/guest"
	"storj.io/drpc/drpcmux"
	"storj.io/drpc/drpcserver"
)

// guestServer implements the DRPC GuestService
type guestServer struct {
	pb.DRPCGuestServiceUnimplementedServer
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

	// Create DRPC server
	mux := drpcmux.New()
	if err := pb.DRPCRegisterGuestService(mux, &guestServer{}); err != nil {
		log.Fatalf("[guest-agent] failed to register service: %v", err)
	}

	server := drpcserver.New(mux)

	// Serve DRPC over vsock - accept connections in a loop
	for {
		conn, err := l.Accept()
		if err != nil {
			log.Printf("[guest-agent] accept error: %v", err)
			continue
		}
		go func() {
			if err := server.ServeOne(context.Background(), conn); err != nil {
				log.Printf("[guest-agent] connection error: %v", err)
			}
		}()
	}
}
