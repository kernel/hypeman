package main

import (
	"context"
	"log"
	"os"

	pb "github.com/kernel/hypeman/lib/guest"
)

// StatPath returns information about a path in the guest filesystem
func (s *guestServer) StatPath(ctx context.Context, req *pb.StatPathRequest) (*pb.StatPathResponse, error) {
	log.Printf("[guest-agent] stat-path: path=%s follow_links=%v", req.Path, req.FollowLinks)

	var info os.FileInfo
	var err error
	if req.FollowLinks {
		info, err = os.Stat(req.Path)
	} else {
		info, err = os.Lstat(req.Path)
	}

	if err != nil {
		if os.IsNotExist(err) {
			return &pb.StatPathResponse{
				Exists: false,
			}, nil
		}
		return &pb.StatPathResponse{
			Exists: false,
			Error:  err.Error(),
		}, nil
	}

	resp := &pb.StatPathResponse{
		Exists: true,
		IsDir:  info.IsDir(),
		IsFile: info.Mode().IsRegular(),
		Mode:   uint32(info.Mode().Perm()),
		Size:   info.Size(),
	}

	// Check if it's a symlink (only relevant if follow_links=false)
	if info.Mode()&os.ModeSymlink != 0 {
		resp.IsSymlink = true
		target, err := os.Readlink(req.Path)
		if err == nil {
			resp.LinkTarget = target
		}
	}

	return resp, nil
}

