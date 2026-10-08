//go:build darwin

package main

import (
	"context"
	"os"
	"os/exec"
	"syscall"

	pb "github.com/kernel/hypeman/lib/guest"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Darwin's launchd is not Linux init: sending it SIGTERM is not a shutdown API.
func (s *guestServer) Shutdown(ctx context.Context, req *pb.ShutdownRequest) (*pb.ShutdownResponse, error) {
	return requestDarwinShutdown(ctx, req, os.Geteuid(), func(ctx context.Context) error {
		return exec.CommandContext(ctx, "/sbin/shutdown", "-h", "now").Run()
	})
}

func requestDarwinShutdown(ctx context.Context, req *pb.ShutdownRequest, euid int, run func(context.Context) error) (*pb.ShutdownResponse, error) {
	if req.Signal != 0 && req.Signal != int32(syscall.SIGTERM) {
		return nil, status.Error(codes.InvalidArgument, "Darwin supports only an orderly shutdown, not arbitrary init signals")
	}
	if euid != 0 {
		return nil, status.Error(codes.PermissionDenied, "Darwin shutdown requires the system guest agent running as root")
	}
	if err := ctx.Err(); err != nil {
		return nil, status.FromContextError(err).Err()
	}
	if err := run(ctx); err != nil {
		return nil, status.Errorf(codes.Internal, "Darwin shutdown failed: %v", err)
	}
	return &pb.ShutdownResponse{}, nil
}
