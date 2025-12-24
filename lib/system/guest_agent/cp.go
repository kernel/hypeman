package main

import (
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"syscall"
	"time"

	pb "github.com/onkernel/hypeman/lib/guest"
)

// CopyToGuest handles copying files to the guest filesystem
func (s *guestServer) CopyToGuest(stream pb.DRPCGuestService_CopyToGuestStream) error {
	log.Printf("[guest-agent] new copy-to-guest stream")

	// Receive start request
	req, err := stream.Recv()
	if err != nil {
		return fmt.Errorf("receive start request: %w", err)
	}

	start := req.GetStart()
	if start == nil {
		return stream.SendAndClose(&pb.CopyToGuestResponse{
			Success: false,
			Error:   "first message must be CopyToGuestStart",
		})
	}

	log.Printf("[guest-agent] copy-to-guest: path=%s mode=%o is_dir=%v size=%d",
		start.Path, start.Mode, start.IsDir, start.Size)

	// Handle directory creation
	if start.IsDir {
		// Check if destination exists and is a file
		if info, err := os.Stat(start.Path); err == nil && !info.IsDir() {
			return stream.SendAndClose(&pb.CopyToGuestResponse{
				Success: false,
				Error:   fmt.Sprintf("cannot create directory: %s is a file", start.Path),
			})
		}

		if err := os.MkdirAll(start.Path, fs.FileMode(start.Mode)); err != nil {
			return stream.SendAndClose(&pb.CopyToGuestResponse{
				Success: false,
				Error:   fmt.Sprintf("create directory: %v", err),
			})
		}
		// Wait for end message
		for {
			req, err := stream.Recv()
			if err == io.EOF {
				break
			}
			if err != nil {
				return stream.SendAndClose(&pb.CopyToGuestResponse{
					Success: false,
					Error:   fmt.Sprintf("receive: %v", err),
				})
			}
			if req.GetEnd() != nil {
				break
			}
		}
		return stream.SendAndClose(&pb.CopyToGuestResponse{
			Success:      true,
			BytesWritten: 0,
		})
	}

	// Create parent directories if needed
	dir := filepath.Dir(start.Path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return stream.SendAndClose(&pb.CopyToGuestResponse{
			Success: false,
			Error:   fmt.Sprintf("create parent directory: %v", err),
		})
	}

	// Check if destination exists and is a directory
	if info, err := os.Stat(start.Path); err == nil && info.IsDir() {
		return stream.SendAndClose(&pb.CopyToGuestResponse{
			Success: false,
			Error:   fmt.Sprintf("cannot copy file: %s is a directory", start.Path),
		})
	}

	// Create file
	file, err := os.OpenFile(start.Path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, fs.FileMode(start.Mode))
	if err != nil {
		return stream.SendAndClose(&pb.CopyToGuestResponse{
			Success: false,
			Error:   fmt.Sprintf("create file: %v", err),
		})
	}
	defer file.Close()

	var bytesWritten int64

	// Receive data chunks
	for {
		req, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			return stream.SendAndClose(&pb.CopyToGuestResponse{
				Success: false,
				Error:   fmt.Sprintf("receive: %v", err),
			})
		}

		if data := req.GetData(); data != nil {
			n, err := file.Write(data)
			if err != nil {
				return stream.SendAndClose(&pb.CopyToGuestResponse{
					Success: false,
					Error:   fmt.Sprintf("write: %v", err),
				})
			}
			bytesWritten += int64(n)
		}

		if req.GetEnd() != nil {
			break
		}
	}

	// Set modification time if provided
	if start.Mtime > 0 {
		mtime := time.Unix(start.Mtime, 0)
		os.Chtimes(start.Path, mtime, mtime)
	}

	// Set ownership if provided (archive mode)
	// Only chown when both UID and GID are explicitly set (non-zero)
	// to avoid accidentally setting one to root (0) when only the other is specified
	if start.Uid > 0 && start.Gid > 0 {
		if err := os.Chown(start.Path, int(start.Uid), int(start.Gid)); err != nil {
			log.Printf("[guest-agent] warning: failed to set ownership on %s: %v", start.Path, err)
		}
	}

	log.Printf("[guest-agent] copy-to-guest complete: %d bytes written to %s", bytesWritten, start.Path)

	return stream.SendAndClose(&pb.CopyToGuestResponse{
		Success:      true,
		BytesWritten: bytesWritten,
	})
}

// CopyFromGuest handles copying files from the guest filesystem
func (s *guestServer) CopyFromGuest(req *pb.CopyFromGuestRequest, stream pb.DRPCGuestService_CopyFromGuestStream) error {
	log.Printf("[guest-agent] copy-from-guest: path=%s follow_links=%v", req.Path, req.FollowLinks)

	// Stat the source path
	var info os.FileInfo
	var err error
	if req.FollowLinks {
		info, err = os.Stat(req.Path)
	} else {
		info, err = os.Lstat(req.Path)
	}
	if err != nil {
		return stream.Send(&pb.CopyFromGuestResponse{
			Response: &pb.CopyFromGuestResponse_Error{
				Error: &pb.CopyFromGuestError{
					Message: fmt.Sprintf("stat: %v", err),
					Path:    req.Path,
				},
			},
		})
	}

	if info.IsDir() {
		// Walk directory and stream all files
		return s.copyFromGuestDir(req.Path, req.FollowLinks, stream)
	}

	// Single file
	return s.copyFromGuestFile(req.Path, "", info, req.FollowLinks, stream, true)
}

// copyFromGuestFile streams a single file
func (s *guestServer) copyFromGuestFile(fullPath, relativePath string, info os.FileInfo, followLinks bool, stream pb.DRPCGuestService_CopyFromGuestStream, isFinal bool) error {
	if relativePath == "" {
		relativePath = filepath.Base(fullPath)
	}

	// Check if it's a symlink
	isSymlink := info.Mode()&os.ModeSymlink != 0
	var linkTarget string
	if isSymlink && !followLinks {
		target, err := os.Readlink(fullPath)
		if err != nil {
			return stream.Send(&pb.CopyFromGuestResponse{
				Response: &pb.CopyFromGuestResponse_Error{
					Error: &pb.CopyFromGuestError{
						Message: fmt.Sprintf("readlink: %v", err),
						Path:    fullPath,
					},
				},
			})
		}
		linkTarget = target
	}

	// Extract UID/GID from file info
	var uid, gid uint32
	if stat, ok := info.Sys().(*syscall.Stat_t); ok {
		uid = stat.Uid
		gid = stat.Gid
	}

	// Send header
	header := &pb.CopyFromGuestHeader{
		Path:       relativePath,
		Mode:       uint32(info.Mode().Perm()),
		IsDir:      false,
		IsSymlink:  isSymlink && !followLinks,
		LinkTarget: linkTarget,
		Size:       info.Size(),
		Mtime:      info.ModTime().Unix(),
		Uid:        uid,
		Gid:        gid,
	}

	if err := stream.Send(&pb.CopyFromGuestResponse{
		Response: &pb.CopyFromGuestResponse_Header{Header: header},
	}); err != nil {
		return err
	}

	// If it's a symlink and we're not following, we're done with this file
	if isSymlink && !followLinks {
		return stream.Send(&pb.CopyFromGuestResponse{
			Response: &pb.CopyFromGuestResponse_End{End: &pb.CopyFromGuestEnd{Final: isFinal}},
		})
	}

	// Stream file content
	file, err := os.Open(fullPath)
	if err != nil {
		return stream.Send(&pb.CopyFromGuestResponse{
			Response: &pb.CopyFromGuestResponse_Error{
				Error: &pb.CopyFromGuestError{
					Message: fmt.Sprintf("open: %v", err),
					Path:    fullPath,
				},
			},
		})
	}
	defer file.Close()

	buf := make([]byte, 32*1024)
	for {
		n, err := file.Read(buf)
		if n > 0 {
			if sendErr := stream.Send(&pb.CopyFromGuestResponse{
				Response: &pb.CopyFromGuestResponse_Data{Data: buf[:n]},
			}); sendErr != nil {
				return sendErr
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return stream.Send(&pb.CopyFromGuestResponse{
				Response: &pb.CopyFromGuestResponse_Error{
					Error: &pb.CopyFromGuestError{
						Message: fmt.Sprintf("read: %v", err),
						Path:    fullPath,
					},
				},
			})
		}
	}

	// Send end marker
	return stream.Send(&pb.CopyFromGuestResponse{
		Response: &pb.CopyFromGuestResponse_End{End: &pb.CopyFromGuestEnd{Final: isFinal}},
	})
}

// copyFromGuestDir walks a directory and streams all files
func (s *guestServer) copyFromGuestDir(rootPath string, followLinks bool, stream pb.DRPCGuestService_CopyFromGuestStream) error {
	// Collect all entries first to know which is final
	type entry struct {
		fullPath     string
		relativePath string
		info         os.FileInfo
	}
	var entries []entry

	err := filepath.WalkDir(rootPath, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			// Send error but continue
			stream.Send(&pb.CopyFromGuestResponse{
				Response: &pb.CopyFromGuestResponse_Error{
					Error: &pb.CopyFromGuestError{
						Message: fmt.Sprintf("walk: %v", err),
						Path:    path,
					},
				},
			})
			return nil
		}

		// Use os.Stat when followLinks is true to get the target's info
		// Use d.Info() (same as os.Lstat) when followLinks is false to get symlink's info
		var info os.FileInfo
		if followLinks {
			info, err = os.Stat(path)
		} else {
			info, err = d.Info()
		}
		if err != nil {
			stream.Send(&pb.CopyFromGuestResponse{
				Response: &pb.CopyFromGuestResponse_Error{
					Error: &pb.CopyFromGuestError{
						Message: fmt.Sprintf("info: %v", err),
						Path:    path,
					},
				},
			})
			return nil
		}

		relPath, _ := filepath.Rel(rootPath, path)
		if relPath == "." {
			relPath = filepath.Base(rootPath)
		} else {
			relPath = filepath.Join(filepath.Base(rootPath), relPath)
		}

		entries = append(entries, entry{
			fullPath:     path,
			relativePath: relPath,
			info:         info,
		})
		return nil
	})
	if err != nil {
		return stream.Send(&pb.CopyFromGuestResponse{
			Response: &pb.CopyFromGuestResponse_Error{
				Error: &pb.CopyFromGuestError{
					Message: fmt.Sprintf("walk directory: %v", err),
					Path:    rootPath,
				},
			},
		})
	}

	// Stream each entry
	for i, e := range entries {
		isFinal := i == len(entries)-1

		if e.info.IsDir() {
			// Extract UID/GID from file info
			var uid, gid uint32
			if stat, ok := e.info.Sys().(*syscall.Stat_t); ok {
				uid = stat.Uid
				gid = stat.Gid
			}

			// Send directory header
			if err := stream.Send(&pb.CopyFromGuestResponse{
				Response: &pb.CopyFromGuestResponse_Header{
					Header: &pb.CopyFromGuestHeader{
						Path:  e.relativePath,
						Mode:  uint32(e.info.Mode().Perm()),
						IsDir: true,
						Mtime: e.info.ModTime().Unix(),
						Uid:   uid,
						Gid:   gid,
					},
				},
			}); err != nil {
				return err
			}
			// Send end for directory
			if err := stream.Send(&pb.CopyFromGuestResponse{
				Response: &pb.CopyFromGuestResponse_End{End: &pb.CopyFromGuestEnd{Final: isFinal}},
			}); err != nil {
				return err
			}
		} else {
			if err := s.copyFromGuestFile(e.fullPath, e.relativePath, e.info, followLinks, stream, isFinal); err != nil {
				return err
			}
		}
	}

	log.Printf("[guest-agent] copy-from-guest complete: %d entries from %s", len(entries), rootPath)
	return nil
}

