package main

import (
	"context"
	"fmt"
	"log"
	"time"

	pb "github.com/kernel/hypeman/lib/guest"
	"golang.org/x/sys/unix"
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
	ts := unix.NsecToTimespec(req.UnixNanos)
	if err := unix.ClockSettime(unix.CLOCK_REALTIME, &ts); err != nil {
		return nil, fmt.Errorf("set realtime clock: %w", err)
	}

	log.Printf("[guest-agent] realtime clock set to %s (adjusted by %s)",
		target.UTC().Format(time.RFC3339Nano), adjustment)
	return &pb.SyncClockResponse{}, nil
}
