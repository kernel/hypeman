package main

import (
	"context"
	"fmt"
	"log"
	"time"

	pb "github.com/kernel/hypeman/lib/guest"
)

// SyncClock sets the guest realtime clock. The guest wall clock resumes from
// the snapshot's saved time after a standby restore, so the host calls this
// post-restore to remove the accumulated skew.
func (s *guestServer) SyncClock(_ context.Context, req *pb.SyncClockRequest) (*pb.SyncClockResponse, error) {
	if req.UnixNanos <= 0 {
		return nil, fmt.Errorf("invalid unix_nanos %d", req.UnixNanos)
	}

	target := time.Unix(0, req.UnixNanos)
	adjustment := time.Until(target)
	if err := setRealtimeClock(req.UnixNanos); err != nil {
		return nil, err
	}

	log.Printf("[guest-agent] realtime clock set to %s (adjusted by %s)",
		target.UTC().Format(time.RFC3339Nano), adjustment)
	return &pb.SyncClockResponse{}, nil
}
