package main

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"

	pb "github.com/kernel/hypeman/lib/guest"
	"github.com/stretchr/testify/require"
)

func TestCancelledExecDoesNotAttemptStart(t *testing.T) {
	for _, tty := range []bool{false, true} {
		t.Run(fmt.Sprintf("tty=%t", tty), func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			stream := &stalledExecStream{ctx: ctx, start: &pb.ExecRequest{Request: &pb.ExecRequest_Start{Start: &pb.ExecStart{Command: []string{filepath.Join(t.TempDir(), "nonexistent-executable")}, Tty: tty}}}}
			err := (&guestServer{}).Exec(stream)
			require.ErrorIs(t, err, context.Canceled, "a canceled request must be rejected before trying to start the command")
		})
	}
}
