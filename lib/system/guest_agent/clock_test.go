package main

import (
	"context"
	"testing"

	pb "github.com/kernel/hypeman/lib/guest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSyncClockRejectsInvalidTime(t *testing.T) {
	s := &guestServer{}

	for _, nanos := range []int64{0, -1} {
		_, err := s.SyncClock(context.Background(), &pb.SyncClockRequest{UnixNanos: nanos})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "invalid unix_nanos")
	}
}
