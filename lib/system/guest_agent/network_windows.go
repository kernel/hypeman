//go:build windows

package main

import (
	"context"

	pb "github.com/kernel/hypeman/lib/guest"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func (s *guestServer) ReconfigureNetwork(context.Context, *pb.ReconfigureNetworkRequest) (*pb.ReconfigureNetworkResponse, error) {
	return nil, status.Error(codes.Unimplemented, "Windows network reconfiguration is not available")
}
