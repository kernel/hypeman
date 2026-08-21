//go:build windows

package main

import (
	"context"
	"fmt"
	"log"
	"os/exec"

	pb "github.com/kernel/hypeman/lib/guest"
)

func (s *guestServer) Shutdown(context.Context, *pb.ShutdownRequest) (*pb.ShutdownResponse, error) {
	log.Printf("[guest-agent] Windows shutdown requested")
	if err := exec.Command("shutdown.exe", "/s", "/t", "0", "/d", "p:0:0").Start(); err != nil {
		return nil, fmt.Errorf("start Windows shutdown: %w", err)
	}
	return &pb.ShutdownResponse{}, nil
}
