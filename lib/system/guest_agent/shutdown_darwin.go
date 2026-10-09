//go:build darwin

package main

import (
	"context"
	"os"
	"os/exec"

	pb "github.com/kernel/hypeman/lib/guest"
)

// Darwin's launchd is not Linux init: sending it SIGTERM is not a shutdown API.
func (s *guestServer) Shutdown(ctx context.Context, req *pb.ShutdownRequest) (*pb.ShutdownResponse, error) {
	return requestDarwinShutdown(ctx, req, os.Geteuid(), func(ctx context.Context) error {
		return exec.CommandContext(ctx, "/sbin/shutdown", "-h", "now").Run()
	})
}
