package main

import (
	"context"
	"log"
	"syscall"

	pb "github.com/kernel/hypeman/lib/guest"
)

// Shutdown sends a signal to PID 1 (init) to trigger graceful shutdown.
// The guest-agent is the messenger -- init owns the process lifecycle and
// will forward the signal to the entrypoint child process.
func (s *guestServer) Shutdown(ctx context.Context, req *pb.ShutdownRequest) (*pb.ShutdownResponse, error) {
	sig := syscall.SIGTERM
	if req.Signal != 0 {
		sig = syscall.Signal(req.Signal)
	}

	log.Printf("[guest-agent] shutdown requested with signal %d (%s)", sig, sig.String())

	if err := syscall.Kill(1, sig); err != nil {
		log.Printf("[guest-agent] failed to signal PID 1: %v", err)
		return nil, err
	}

	return &pb.ShutdownResponse{}, nil
}
